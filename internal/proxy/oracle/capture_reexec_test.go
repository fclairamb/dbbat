//go:build capture

// Capture tooling for the cursor re-execution fixtures.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_.*Reexec -v ./internal/proxy/oracle/
//
// Each test drives a client that re-runs an already-parsed statement without
// resending its text, recording every TNS packet. See
// cursor_reexec_capture_test.go for what the recordings are asserted against.
package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// reexecQuery is deliberately trivial and side-effect free: the fixture is
// about the *frame* a re-execution puts on the wire, not about what it returns.
const reexecQuery = "SELECT 1 AS n FROM dual"

// reexecRuns is how many times each client re-runs the prepared statement. The
// first run parses (and carries the SQL); every run after it is the shape we
// are hunting.
const reexecRuns = 4

// requireOracleReachable skips the test when no Oracle answers at addr.
func requireOracleReachable(t *testing.T, addr string) {
	t.Helper()

	probe, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", addr, err)
	}

	_ = probe.Close()
}

// newCaptureWriter opens a dump writer for a capture, failing the test if the
// path cannot be written.
func newCaptureWriter(t *testing.T, outPath, sessionID string) *dump.Writer {
	t.Helper()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: sessionID,
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	return w
}

// TestCapture_GoOraCursorReexec records go-ora running one prepared statement
// several times over a single session. go-ora clears its `parse` flag after the
// first execution (`basicWrite` in command.go), so runs 2..N put a piggyback
// exec (func 0x03, sub 0x5e) on the wire carrying the cursor id and a
// zero-length SQL field — a cursor re-execution as a real client emits it.
func TestCapture_GoOraCursorReexec(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_REEXEC", "testdata/go_ora_cursor_reexec.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-cursor-reexec")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1) // one session in the dump, so one cursor id

	ctx := t.Context()

	// A real prepared statement: database/sql keeps the same driver stmt, which
	// is what lets go-ora reuse the cursor instead of re-parsing.
	stmt, err := db.PrepareContext(ctx, reexecQuery)
	require.NoError(t, err)

	for i := range reexecRuns {
		var n int

		require.NoError(t, stmt.QueryRowContext(ctx).Scan(&n), "run %d", i+1)
		require.Equal(t, 1, n)
	}

	require.NoError(t, stmt.Close())
	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d executions)", outPath, reexecRuns)
}

// TestCapture_GoOraDMLCursorReexec records go-ora re-running a prepared DML.
// go-ora splits the two cases (`write` in command.go): a re-executed SELECT is
// piggyback sub-op 0x4e, a re-executed DML is sub-op 0x04. The DML shape is the
// one `read_only` and `block_ddl` care about, so it gets its own fixture.
func TestCapture_GoOraDMLCursorReexec(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_REEXEC_DML", "testdata/go_ora_dml_cursor_reexec.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-dml-cursor-reexec")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	_, _ = db.ExecContext(ctx, "DROP TABLE dbbat_reexec_test") // ignore ORA-00942
	_, err = db.ExecContext(ctx, "CREATE TABLE dbbat_reexec_test (id NUMBER)")
	require.NoError(t, err)

	defer func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE dbbat_reexec_test") }()

	stmt, err := db.PrepareContext(ctx, "INSERT INTO dbbat_reexec_test VALUES (1)")
	require.NoError(t, err)

	for i := range reexecRuns {
		_, err := stmt.ExecContext(ctx)
		require.NoError(t, err, "run %d", i+1)
	}

	require.NoError(t, stmt.Close())

	_, err = db.ExecContext(ctx, "DROP TABLE dbbat_reexec_test")
	require.NoError(t, err)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d executions)", outPath, reexecRuns)
}

// pythonReexecScript re-executes the same statement on the same cursor object.
// python-oracledb thin caches the parsed statement per connection, so the
// second and later executes are sent without the statement text.
const pythonReexecScript = `
import sys, oracledb
dsn = sys.argv[1]
runs = int(sys.argv[2])
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    with conn.cursor() as cur:
        for _ in range(runs):
            cur.execute("SELECT 1 AS n FROM dual")
            assert cur.fetchone()[0] == 1
print("ok")
`

// TestCapture_PythonThinCursorReexec records python-oracledb (thin) executing
// the same statement repeatedly on one cursor. Skipped when the interpreter or
// the driver is not installed — this is capture tooling, not CI.
func TestCapture_PythonThinCursorReexec(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_REEXEC_PY", "testdata/python_thin_cursor_reexec.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-cursor-reexec")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonReexecScript)

	cmd := exec.CommandContext(t.Context(), python, script,
		fmt.Sprintf("%s/%s", relayAddr, oracleService), fmt.Sprint(reexecRuns))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d executions)", outPath, reexecRuns)
}

// writeTempScript writes body to a temporary .py file and returns its path.
func writeTempScript(t *testing.T, body string) string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "reexec-*.py")
	require.NoError(t, err)

	_, err = f.WriteString(body)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return f.Name()
}
