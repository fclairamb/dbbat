//go:build capture

// Capture tooling for the array-DML and chunked-CLR fixture.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_PythonThinArrayDML -v ./internal/proxy/oracle/
//
// The recording is what array_dml_replay_test.go replays. Every value in it is
// synthetic.
package oracle

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pythonArrayDMLScript drives the shapes the fixture exists for, on one
// session:
//
//  1. executemany: one `03 5e` parse+execute carrying every bind row
//     (al8i4[1] = 30 iterations, 30 rows of `07` + values).
//  2. executemany again on the cursor that is now parsed: a `03 04`
//     re-execution carrying every bind row.
//  3. the same UPDATE in a cursor.execute() loop: one `03 04` re-execution per
//     row, each carrying its own bind row. This is the shape that was logged
//     as one statement with the first row's binds.
//  4. a SELECT re-executed with fresh binds: a `03 4e` carrying a bind row.
//  5. a SELECT returning VARCHAR2 values past 252 bytes, which the server
//     sends in the CLR long form (`fe`, chunks, `00`) — the form a server
//     whose character set differs from the client's also uses for short
//     values with non-ASCII text in them.
const pythonArrayDMLScript = `
import sys, oracledb
dsn = sys.argv[1]
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    cur = conn.cursor()
    cur.execute("BEGIN EXECUTE IMMEDIATE 'DROP TABLE dbbat_array_dml'; EXCEPTION WHEN OTHERS THEN NULL; END;")
    cur.execute("CREATE TABLE dbbat_array_dml (id NUMBER PRIMARY KEY, name VARCHAR2(60), note VARCHAR2(400))")
    cur.executemany("INSERT INTO dbbat_array_dml (id, name) VALUES (:1, :2)",
                    [(i, "Bloc %d" % i) for i in range(1, 61)])
    conn.commit()

    upd = "UPDATE dbbat_array_dml SET name = :nouveau WHERE id = :n AND name = :ancien"

    # 1. executemany: 30 rows, 29 of which match (id 13 names a row that is not there)
    cur.executemany(upd, [{"nouveau": "Bloc %d bis" % i, "n": i if i != 13 else 999, "ancien": "Bloc %d" % i}
                          for i in range(1, 31)])
    print("executemany parse rowcount", cur.rowcount)

    # 2. executemany again, now a re-execution: 10 rows, all matching
    cur.executemany(upd, [{"nouveau": "Bloc %d bis" % i, "n": i, "ancien": "Bloc %d" % i}
                          for i in range(31, 41)])
    print("executemany reexec rowcount", cur.rowcount)
    conn.commit()

    # 3. the same statement, one execute() per row: 5 rows, 4 of which match
    for i in (41, 42, 43, 44, 999):
        cur.execute(upd, nouveau="Bloc %d ter" % i, n=i, ancien="Bloc %d" % i)
        print("execute rowcount", cur.rowcount)
    conn.commit()

    # 4. a SELECT re-executed with fresh binds
    for lo in (1, 41):
        cur.execute("SELECT id, name FROM dbbat_array_dml WHERE id BETWEEN :lo AND :hi ORDER BY id",
                    lo=lo, hi=lo + 4)
        print("select rows", len(cur.fetchall()))

    # 5. VARCHAR2 values past the 252-byte short form, accented ones included
    cur.execute("UPDATE dbbat_array_dml SET note = RPAD('Bâtiment ' || id, 300, '.') WHERE id <= 6")
    conn.commit()
    cur.execute("SELECT id, note, name FROM dbbat_array_dml WHERE id <= 8 ORDER BY id")
    got = cur.fetchall()
    assert got[0][1].startswith("Bâtiment 1"), got[0]
    print("long rows", len(got))

    cur.execute("DROP TABLE dbbat_array_dml")
print("ok")
`

// TestCapture_PythonThinArrayDML records pythonArrayDMLScript. Skipped when the
// interpreter or the driver is not installed — this is capture tooling, not CI.
func TestCapture_PythonThinArrayDML(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_ARRAY_DML", "testdata/python_thin_array_dml.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-array-dml")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonArrayDMLScript)

	cmd := exec.CommandContext(t.Context(), python, script, fmt.Sprintf("%s/%s", relayAddr, oracleService))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}
