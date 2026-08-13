//go:build capture

// Capture tooling for the "a statement failed" fixtures.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_PythonThinFailedStatements -v ./internal/proxy/oracle/
//
// The question these recordings answer is narrow: *how does a failure arrive* —
// as an embedded OER inside the execute Response, or as a standalone func=0x04
// message after a marker exchange, and with or without the end-of-call bit?
// The answers are asserted in failed_stmt_replay_test.go, chiefly
// TestDumpReplay_FailuresArriveAsBitLessStandaloneOERs.
package oracle

import (
	"database/sql"
	"fmt"
	"os/exec"
	"testing"
	"time"

	_ "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/require"
)

// pythonFailedStmtScript runs three statements that fail in three different
// ways, on one python-oracledb thin session:
//
//   - a SELECT against a table that does not exist (ORA-00942), which fails at
//     parse time;
//   - an INSERT violating a unique constraint (ORA-00001), which parses fine
//     and fails during execution — the DML case the todo is about;
//   - a DROP of a missing table (ORA-00942 again) for the DDL shape;
//   - a SELECT that parses but dies while executing (ORA-01476, divide by
//     zero), which is the shape closest to a mid-fetch failure;
//   - an anonymous PL/SQL block that raises (ORA-06512 under ORA-20001) and one
//     that will not compile (ORA-06550 carrying PLS- text), because those are
//     the diagnostics whose message text does not begin with the ORA- code the
//     OER's errNum reports.
//
// Each failure is caught so the session survives to produce the next one.
const pythonFailedStmtScript = `
import sys, oracledb
dsn = sys.argv[1]

def failing(cur, label, sql):
    try:
        cur.execute(sql)
        print(label, ": DID-NOT-FAIL")
    except Exception as e:
        print(label, ":", str(e).strip().splitlines()[0])

with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    with conn.cursor() as cur:
        failing(cur, "drop-setup", "DROP TABLE dbbat_fail_test")
        cur.execute("CREATE TABLE dbbat_fail_test (id NUMBER PRIMARY KEY)")
        cur.execute("INSERT INTO dbbat_fail_test VALUES (1)")
        conn.commit()
        failing(cur, "select-missing-table", "SELECT * FROM dbbat_no_such_table_xyz")
        failing(cur, "insert-dup", "INSERT INTO dbbat_fail_test VALUES (1)")
        failing(cur, "drop-missing", "DROP TABLE dbbat_no_such_table_xyz")
        failing(cur, "select-divzero", "SELECT 1/0 FROM dual")
        failing(cur, "plsql-raise",
                "BEGIN RAISE_APPLICATION_ERROR(-20001, 'dbbat measured this'); END;")
        failing(cur, "plsql-badcompile", "BEGIN dbbat_no_such_procedure_xyz; END;")
        cur.execute("DROP TABLE dbbat_fail_test")
print("ok")
`

// TestCapture_PythonThinFailedStatements records the session described above.
// Skipped when the interpreter or the driver is not installed — this is capture
// tooling, not CI.
func TestCapture_PythonThinFailedStatements(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_FAIL_PY", "testdata/python_thin_failed_stmt.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-failed-stmt")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonFailedStmtScript)

	cmd := exec.CommandContext(t.Context(), python, script,
		fmt.Sprintf("%s/%s", relayAddr, oracleService))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// TestCapture_GoOraFailedStatements mirrors the script above on go-ora, the
// client whose OERs *do* carry the end-of-call bit. It exists so the comparison
// in the replay test is between two measurements rather than between one
// measurement and an assumption.
func TestCapture_GoOraFailedStatements(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_FAIL_GOORA", "testdata/go_ora_failed_stmt.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-failed-stmt")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService)

	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1) // one session in the dump

	ctx := t.Context()

	statements := []struct {
		sql       string
		query     bool // driven through QueryContext, so a fetch-time failure surfaces
		expectErr bool
	}{
		{sql: "DROP TABLE dbbat_fail_test"}, // ORA-00942 on a clean database, ignored
		{sql: "CREATE TABLE dbbat_fail_test (id NUMBER PRIMARY KEY)"},
		{sql: "INSERT INTO dbbat_fail_test VALUES (1)"},
		{sql: "SELECT 1 FROM dbbat_no_such_table_xyz", query: true, expectErr: true},
		{sql: "INSERT INTO dbbat_fail_test VALUES (1)", expectErr: true},
		{sql: "DROP TABLE dbbat_no_such_table_xyz", expectErr: true},
		{sql: "SELECT 1/0 FROM dual", query: true, expectErr: true},
		{sql: "BEGIN RAISE_APPLICATION_ERROR(-20001, 'dbbat measured this'); END;", expectErr: true},
		{sql: "BEGIN dbbat_no_such_procedure_xyz; END;", expectErr: true},
		{sql: "DROP TABLE dbbat_fail_test"},
	}

	for i, st := range statements {
		var err error

		if st.query {
			var n int
			err = db.QueryRowContext(ctx, st.sql).Scan(&n)
		} else {
			_, err = db.ExecContext(ctx, st.sql)
		}

		if st.expectErr {
			require.Errorf(t, err, "statement %d (%s) was supposed to fail", i, st.sql)
			t.Logf("go-ora %s -> %v", st.sql, err)
		}
	}

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}
