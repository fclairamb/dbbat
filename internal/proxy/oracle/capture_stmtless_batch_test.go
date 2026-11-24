//go:build capture

// Capture tooling for the statement-less `03 5e` batch fixture.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	PYTHON_BIN=/tmp/orav/bin/python go test -tags capture -timeout 300s -run TestCapture_PythonThinStatementlessBatch -v ./internal/proxy/oracle/
//
// Every value in the recording is synthetic.
package oracle

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pythonStatementlessBatchScript runs an executemany, then the same statement
// again with values longer than the buffers the first call declared: the client
// re-executes the cursor through the full execute op with no statement text,
// carrying every bind row.
const pythonStatementlessBatchScript = `
import sys, oracledb
dsn = sys.argv[1]
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    cur = conn.cursor()
    cur.execute("BEGIN EXECUTE IMMEDIATE 'DROP TABLE dbbat_stmtless'; EXCEPTION WHEN OTHERS THEN NULL; END;")
    cur.execute("CREATE TABLE dbbat_stmtless (id NUMBER PRIMARY KEY, name VARCHAR2(200))")
    ins = "INSERT INTO dbbat_stmtless (id, name) VALUES (:1, :2)"
    cur.executemany(ins, [(i, "n%d" % i) for i in range(1, 6)])
    print("short rowcount", cur.rowcount)
    cur.executemany(ins, [(i, ("longer value %d " % i) * 4) for i in range(6, 14)])
    print("long rowcount", cur.rowcount)
    conn.commit()
    cur.execute("DROP TABLE dbbat_stmtless")
print("ok")
`

// TestCapture_PythonThinStatementlessBatch records pythonStatementlessBatchScript.
func TestCapture_PythonThinStatementlessBatch(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_STMTLESS_BATCH", "testdata/python_thin_stmtless_batch.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-stmtless-batch")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonStatementlessBatchScript)

	cmd := exec.CommandContext(t.Context(), python, script, fmt.Sprintf("%s/%s", relayAddr, oracleService))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond)
	require.NoError(t, w.Close())
}
