//go:build capture

// Capture tooling for the **thin** dialect's half of the LOB evidence.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_GoOraLOB ./internal/proxy/oracle/
//
// The two OCI dialects are recorded by TestCapture_OCILOBFetchThroughDBBat
// (`-tags integration`); this is the compressed encoding a thin client speaks,
// and it goes through a bare relay because nothing in it depends on what dbbat
// rewrites — see ociFixtureProvenance for why the 64-bit one cannot.
package oracle

import (
	"database/sql"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCapture_GoOraLOBInline records ociLOBQuery driven by go-ora with its
// **default** LOB policy, and the default is the finding: a thin client asks
// for the LOB bodies up front, so the server inlines them and the row carries
// no locator to step over at all.
func TestCapture_GoOraLOBInline(t *testing.T) {
	captureGoOraLOB(t, "capture-go-ora-lob", "testdata/"+goOraLOBFixture, "")
}

// TestCapture_GoOraLOBStream records the same query with `lob fetch=post`,
// which is the thin client asking for locators instead — the shape the row walk
// has framing to skip in, and therefore the one that says whether a thin
// session spells that framing the way the 64-bit OCI one does.
func TestCapture_GoOraLOBStream(t *testing.T) {
	captureGoOraLOB(t, "capture-go-ora-lob-stream", "testdata/"+goOraLOBStreamFixture,
		"?lob+fetch=post&lob+read=no")
}

// pythonLOBScript runs goOraLOBQuery on python-oracledb thin with **nothing
// configured**, which is the whole point of the recording: it says what a
// second, independently written thin driver asks for when the application says
// nothing about LOBs.
//
// `oracledb.defaults.fetch_lobs` is left alone (it is True), so the driver
// fetches LOB objects — handles — and each value is printed by type rather than
// read, because reading one would issue LOB reads of its own and those are not
// what this fixture is about.
const pythonLOBScript = `
import sys, oracledb
dsn = sys.argv[1]
sql = sys.argv[2]

with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    with conn.cursor() as cur:
        cur.execute(sql)
        cols = [d[0] for d in cur.description]
        print("columns:", cols)
        n = 0
        for row in cur:
            n += 1
            for name, value in zip(cols, row):
                print("  %s = %s %r" % (name, type(value).__name__, value))
        print("rows:", n)
print("ok")
`

// TestCapture_PythonThinLOB records goOraLOBQuery on python-oracledb thin with
// its own default LOB policy.
//
// It is the third thin recording and the one that says whether go-ora's inline
// default is the thin dialect's norm or go-ora's own habit — which is the fact
// the row walk's default reading rests on, and which two recordings of one
// driver cannot establish.
func TestCapture_PythonThinLOB(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LOB_PY", "testdata/"+pythonThinLOBFixture)
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-lob")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonLOBScript)

	cmd := exec.CommandContext(t.Context(), python, script,
		fmt.Sprintf("%s/%s", relayAddr, oracleService), goOraLOBQuery)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// captureGoOraLOB runs ociLOBQuery through a recording relay and writes the
// session to outPath. Values are logged rather than asserted: what the columns
// scan into is the driver's business, and the fixture is about the bytes.
func captureGoOraLOB(t *testing.T, sessionID, outPath, dsnOptions string) {
	t.Helper()

	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, sessionID)
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s%s", relayAddr, oracleService, dsnOptions)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	rows, err := db.QueryContext(t.Context(), goOraLOBQuery)
	require.NoError(t, err)

	cols, err := rows.Columns()
	require.NoError(t, err)
	require.Len(t, cols, len(goOraLOBColumns), "the fixture is worthless at any other column count")

	t.Logf("columns: %v", cols)

	got := 0

	for rows.Next() {
		values := make([]interface{}, len(cols))
		into := make([]interface{}, len(cols))

		for i := range values {
			into[i] = &values[i]
		}

		require.NoError(t, rows.Scan(into...))

		for i, v := range values {
			t.Logf("  %s = %T %v", cols[i], v, v)
		}

		got++
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 1, got, "ociLOBQuery selects from dual and returns exactly one row")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}
