//go:build capture

// Capture tooling for the "a statement failed *after* rows started flowing"
// fixtures.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 600s -run 'TestCapture_.*MidFetchFailure' -v ./internal/proxy/oracle/
//
// The question is the one capture_failed_stmt_test.go left open. Every failure
// in those two recordings is raised *before* the first row, so the server sends
// the OER instead of the QueryResult and dbbat is never in a row stream when it
// arrives. Here the failure is raised deep inside the fetch — a TO_NUMBER of a
// row far past the first array — so the QueryResult, its column definitions and
// several fetch round trips are already behind it.
//
// The answers are asserted in midfetch_fail_replay_test.go.
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

// midFetchRowCount is how many rows the fixture table holds, and
// midFetchBadRow is the one whose text will not convert. The bad row sits far
// enough in that the client has issued dozens of fetch round trips — and dbbat
// has decoded the column definitions and captured rows — before the server
// raises anything.
const (
	midFetchRowCount = 20000
	midFetchBadRow   = 15000
	midFetchArray    = 100
)

// midFetchDDL builds the fixture table. No ORDER BY is used on the SELECT that
// follows, deliberately: a sort would materialize the whole result set and the
// conversion would blow up before the first row, which is the shape already
// measured.
func midFetchDDL() []string {
	return []string{
		"CREATE TABLE dbbat_midfetch (id NUMBER, txt VARCHAR2(30))",
		fmt.Sprintf(`INSERT INTO dbbat_midfetch
			SELECT level, CASE WHEN level = %d THEN 'not-a-number' ELSE TO_CHAR(level) END
			FROM dual CONNECT BY level <= %d`, midFetchBadRow, midFetchRowCount),
	}
}

// midFetchQuery fails with ORA-01722 while producing row midFetchBadRow.
const midFetchQuery = "SELECT id, TO_NUMBER(txt) AS n FROM dbbat_midfetch"

// pythonMidFetchScript drives the failure on python-oracledb thin. prefetchrows
// is pinned to 0 so every batch is a real fetch round trip rather than something
// the driver piggybacked onto the execute.
const pythonMidFetchScript = `
import sys, oracledb
dsn, ddl1, ddl2, query, arraysize, = sys.argv[1:6]

with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    with conn.cursor() as cur:
        try:
            cur.execute("DROP TABLE dbbat_midfetch")
        except Exception as e:
            print("drop-setup :", str(e).strip().splitlines()[0])
        cur.execute(ddl1)
        cur.execute(ddl2)
        conn.commit()

        cur.arraysize = int(arraysize)
        cur.prefetchrows = 0
        cur.execute(query)
        n = 0
        try:
            for _ in cur:
                n += 1
            print("midfetch : DID-NOT-FAIL after", n, "rows")
        except Exception as e:
            print("midfetch :", n, "rows then", str(e).strip().splitlines()[0])
        cur.execute("DROP TABLE dbbat_midfetch")
print("ok")
`

// TestCapture_PythonThinMidFetchFailure records a python-oracledb thin session
// that fetches its way into a failure.
func TestCapture_PythonThinMidFetchFailure(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_MIDFETCH_PY", "testdata/python_thin_midfetch_fail.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-midfetch-fail")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonMidFetchScript)
	ddl := midFetchDDL()

	cmd := exec.CommandContext(t.Context(), python, script,
		fmt.Sprintf("%s/%s", relayAddr, oracleService),
		ddl[0], ddl[1], midFetchQuery, fmt.Sprint(midFetchArray))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// TestCapture_GoOraMidFetchFailure mirrors it on go-ora, for the same reason the
// failed-statement fixtures are recorded twice: so the conclusion rests on two
// measurements rather than on one plus an assumption about the other client.
func TestCapture_GoOraMidFetchFailure(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_MIDFETCH_GOORA", "testdata/go_ora_midfetch_fail.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-midfetch-fail")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=%d", relayAddr, oracleService, midFetchArray)

	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1) // one session in the dump

	ctx := t.Context()

	_, _ = db.ExecContext(ctx, "DROP TABLE dbbat_midfetch") // ignored on a clean database

	for _, stmt := range midFetchDDL() {
		_, err = db.ExecContext(ctx, stmt)
		require.NoErrorf(t, err, "setup: %s", stmt)
	}

	rows, err := db.QueryContext(ctx, midFetchQuery)
	require.NoError(t, err, "the execute itself must succeed — the failure has to be raised mid-fetch")

	var seen int

	for rows.Next() {
		var id, n float64

		require.NoError(t, rows.Scan(&id, &n))

		seen++
	}

	err = rows.Err()
	require.Errorf(t, err, "the fetch was supposed to fail; it returned %d rows cleanly", seen)
	t.Logf("go-ora: %d rows then %v", seen, err)

	require.NoError(t, rows.Close())

	_, err = db.ExecContext(ctx, "DROP TABLE dbbat_midfetch")
	require.NoError(t, err)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}
