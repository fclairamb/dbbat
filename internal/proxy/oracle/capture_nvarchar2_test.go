//go:build capture

// Capture tooling for the NVARCHAR2 / NCHAR fixtures.
//
// Usage:
//
//	docker run -d --name dbbat-ora-nchar -p 51523:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	ORACLE_ADDR=localhost:51523 go test -tags capture -timeout 300s -run 'TestCapture_.*NVarchar2' -v ./internal/proxy/oracle/
//
// The recordings are what national_charset_replay_test.go replays. Every value
// is synthetic.
package oracle

import (
	"database/sql"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// nvarchar2Setup creates a table holding the same text in a VARCHAR2, an
// NVARCHAR2 and an NCHAR column. The database national character set of Oracle
// Free is AL16UTF16, so the national columns travel as UTF-16BE.
const nvarchar2Create = "CREATE TABLE dbbat_nchar (id NUMBER PRIMARY KEY, v VARCHAR2(40), nv NVARCHAR2(40), nc NCHAR(12))"

const nvarchar2Insert = "INSERT INTO dbbat_nchar (id, v, nv, nc) VALUES (1, 'Bâtiment 1', N'Bâtiment 1', N'Bâtiment 1')"

const nvarchar2Select = "SELECT id, v, nv, nc FROM dbbat_nchar ORDER BY id"

const pythonNVarchar2Script = `
import sys, oracledb
dsn = sys.argv[1]
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    cur = conn.cursor()
    cur.execute("BEGIN EXECUTE IMMEDIATE 'DROP TABLE dbbat_nchar'; EXCEPTION WHEN OTHERS THEN NULL; END;")
    cur.execute("` + nvarchar2Create + `")
    cur.execute("` + nvarchar2Insert + `")
    conn.commit()
    cur.execute("` + nvarchar2Select + `")
    got = cur.fetchall()
    assert got[0][2] == "Bâtiment 1", got
    print("rows", got)
    cur.execute("DROP TABLE dbbat_nchar")
print("ok")
`

// TestCapture_PythonThinNVarchar2 records pythonNVarchar2Script.
func TestCapture_PythonThinNVarchar2(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_PY_NVARCHAR2", "testdata/python_thin_nvarchar2.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-nvarchar2")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonNVarchar2Script)

	out, err := exec.CommandContext(t.Context(), python, script, fmt.Sprintf("%s/%s", relayAddr, oracleService)).CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond)
	require.NoError(t, w.Close())
}

// TestCapture_GoOraNVarchar2 records the same statements through go-ora.
func TestCapture_GoOraNVarchar2(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_GO_NVARCHAR2", "testdata/go_ora_nvarchar2.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-nvarchar2")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	db, err := sql.Open("oracle", fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService))
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	_, _ = db.ExecContext(ctx, "BEGIN EXECUTE IMMEDIATE 'DROP TABLE dbbat_nchar'; EXCEPTION WHEN OTHERS THEN NULL; END;")
	_, err = db.ExecContext(ctx, nvarchar2Create)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, nvarchar2Insert)
	require.NoError(t, err)

	var (
		id         int
		v, nv, nch string
	)

	require.NoError(t, db.QueryRowContext(ctx, nvarchar2Select).Scan(&id, &v, &nv, &nch))
	require.Equal(t, "Bâtiment 1", nv)

	_, err = db.ExecContext(ctx, "DROP TABLE dbbat_nchar")
	require.NoError(t, err)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, w.Close())
}
