//go:build capture

// Capture tooling for the **thin** dialect's half of the LOB evidence, and for
// the LONG columns that turned out to be the same shape one type family over.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_GoOraLOB ./internal/proxy/oracle/
//	go test -tags capture -timeout 300s -run TestCapture_JDBCThinLOB ./internal/proxy/oracle/
//	go test -tags capture -timeout 300s -run 'TestCapture_(GoOra|PythonThin)Long' ./internal/proxy/oracle/
//
// The two OCI dialects are recorded by TestCapture_OCILOBFetchThroughDBBat
// (`-tags integration`); this is the compressed encoding a thin client speaks,
// and it goes through a bare relay because nothing in it depends on what dbbat
// rewrites — see ociFixtureProvenance for why the 64-bit one cannot.
package oracle

import (
	"database/sql"
	"fmt"
	"os"
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

// TestCapture_GoOraLOBInlineBig records the same client and the same default
// policy on a CLOB of 300 characters — forty-eight past the 252 a CLR can carry
// behind a single length byte.
//
// Every LOB in goOraLOBQuery is under that limit, so the inlined value always
// arrived in the short form and the reading was a single length byte for as
// long as it was. This is the recording that says what happens on the other
// side of it. See readInlineLongColumn.
func TestCapture_GoOraLOBInlineBig(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LOB_BIG", "testdata/"+goOraBigLOBFixture)

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-lob-big")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	db, err := sql.Open("oracle",
		fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService))
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)
	logCapturedRows(t, db, goOraBigLOBQuery)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
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

// jdbcLOBSource runs goOraLOBQuery on Oracle's own JDBC thin driver with
// **nothing configured**, which is the whole point of the recording: JDBC thin
// prefetches LOB data by default (`oracle.jdbc.defaultLobPrefetchSize`), so it
// is the one major thin client that had a documented reason to ask for the
// bodies and no recording to say whether it does.
//
// Each value is reported by class name rather than read: reading a Clob would
// issue LOB reads of its own, and those are not what this fixture is about.
const jdbcLOBSource = `
import java.sql.*;
public class Lob {
  public static void main(String[] a) throws Exception {
    try (Connection c = DriverManager.getConnection("jdbc:oracle:thin:@//" + a[0], "system", "oracle");
         Statement s = c.createStatement();
         ResultSet rs = s.executeQuery(a[1])) {
      ResultSetMetaData md = rs.getMetaData();
      int n = md.getColumnCount();
      StringBuilder cols = new StringBuilder();
      for (int i = 1; i <= n; i++) cols.append(md.getColumnName(i)).append(":").append(md.getColumnTypeName(i)).append(" ");
      System.out.println("columns: " + cols);
      int rows = 0;
      while (rs.next()) {
        rows++;
        for (int i = 1; i <= n; i++) {
          Object v = rs.getObject(i);
          System.out.println("  " + md.getColumnName(i) + " = " + (v == null ? "null" : v.getClass().getName()));
        }
      }
      System.out.println("rows: " + rows);
    }
    System.out.println("ok");
  }
}
`

// TestCapture_JDBCThinLOB records goOraLOBQuery on JDBC thin with its own
// default LOB policy.
//
// It is the fourth thin recording and the third thin driver, and the one the
// other three could not answer for: go-ora's default inlines, python-oracledb
// thin's default fetches locators, and JDBC's documented default prefetch said
// nothing about which define block it writes to get there.
func TestCapture_JDBCThinLOB(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LOB_JDBC", "testdata/"+jdbcThinLOBFixture)
	jar := captureEnv("OJDBC_JAR", "/opt/homebrew/Caskroom/sqlcl/26.1.0.086.1709/sqlcl/lib/ojdbc11.jar")

	requireOracleReachable(t, oracleAddr)

	if _, err := os.Stat(jar); err != nil {
		t.Skipf("ojdbc jar not found at %s: %v", jar, err)
	}

	if _, err := exec.LookPath("javac"); err != nil {
		t.Skipf("javac unavailable: %v", err)
	}

	dir := t.TempDir()
	src := dir + "/Lob.java"
	require.NoError(t, os.WriteFile(src, []byte(jdbcLOBSource), 0o600))

	if out, err := exec.Command("javac", "-d", dir, src).CombinedOutput(); err != nil {
		t.Skipf("javac failed: %v (%s)", err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-jdbc-thin-lob")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	cmd := exec.CommandContext(t.Context(), "java", "-cp", dir+":"+jar, "Lob",
		fmt.Sprintf("%s/%s", relayAddr, oracleService), goOraLOBQuery)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "jdbc client failed: %s", out)
	t.Logf("jdbc client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// longTableDropDDL and longTableDDL build the two tables longQuery and
// longRawQuery select from. They run on the **same session** the fetch does, which is why
// the recordings carry their setup: go-ora opens exactly one working connection
// per process against this server (a second one is answered with EOF, measured
// 2026-09-22 on gvenzl/oracle-free:23-slim), so a tidier setup connection of its
// own would cost the capture the session it is there to record. The replay
// picks the fetch out by SQL marker, so the extra frames cost nothing.
//
// The drops are a list of their own because their failure is expected — the
// capture is re-run against a container that may or may not already hold the
// tables.
var (
	longTableDropDDL = []string{
		`DROP TABLE dbbat_cap_long`,
		`DROP TABLE dbbat_cap_longraw`,
	}

	longTableDDL = []string{
		`CREATE TABLE dbbat_cap_long (n NUMBER, l1 LONG)`,
		`CREATE TABLE dbbat_cap_longraw (n NUMBER, r1 LONG RAW)`,
		`INSERT INTO dbbat_cap_long VALUES (1, 'longvalue-0123456789')`,
		`INSERT INTO dbbat_cap_long VALUES (2, NULL)`,
		`INSERT INTO dbbat_cap_longraw VALUES (1, HEXTORAW('DEADBEEF'))`,
		`INSERT INTO dbbat_cap_longraw VALUES (2, NULL)`,
	}
)

// TestCapture_GoOraLong records longQuery and longRawQuery — columns the
// **describe** reports as LONG (8) and LONG RAW (24), rather than a LOB a
// client re-declared as one — driven by go-ora with nothing configured.
//
// It needs the two tables longTableDDL builds, which is what makes it the one
// capture in this file with setup in front of it: Oracle allows a single LONG
// column per table and none at all in a `FROM dual` expression list.
func TestCapture_GoOraLong(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LONG", "testdata/"+goOraLongFixture)

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-long")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	db, err := sql.Open("oracle",
		fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService))
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)
	setupLongTables(t, db)

	for _, query := range []string{longQuery, longRawQuery} {
		logCapturedRows(t, db, query)
	}

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// pythonLongScript runs both LONG queries on python-oracledb thin with nothing
// configured, which is the same ask the go-ora recording makes: what a second,
// independently written thin driver does with a column the describe reports as
// a LONG.
//
// The argument list is the DDL, then a lone "--", then the two queries — the
// setup runs on the recorded session for the same reason the go-ora capture's
// does (see longTableDDL), and a statement that fails before the separator is
// one of the drops, whose failure is expected.
const pythonLongScript = `
import sys, oracledb
dsn = sys.argv[1]
setup = sys.argv[2:sys.argv.index("--")]
queries = sys.argv[sys.argv.index("--") + 1:]
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    for sql in setup:
        with conn.cursor() as cur:
            try:
                cur.execute(sql)
            except Exception as e:
                print("setup:", sql, "->", e)
    conn.commit()
    for sql in queries:
        with conn.cursor() as cur:
            cur.execute(sql)
            print("columns:", [(d[0], str(d[1])) for d in cur.description])
            for row in cur:
                print("  ", repr(row))
print("ok")
`

// TestCapture_PythonThinLong is TestCapture_GoOraLong on the second thin
// driver. The two disagree about how a LOB column is framed, so nothing said
// they agree about a LONG one either, and one recording could not tell.
func TestCapture_PythonThinLong(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LONG_PY", "testdata/"+pythonThinLongFixture)
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-long")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonLongScript)

	args := []string{script, fmt.Sprintf("%s/%s", relayAddr, oracleService)}
	args = append(args, longTableDropDDL...)
	args = append(args, longTableDDL...)
	args = append(args, "--", longQuery, longRawQuery)

	cmd := exec.CommandContext(t.Context(), python, args...)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// setupLongTables runs longTableDDL on the session that is about to be
// recorded. See longTableDDL for why it is not a connection of its own.
func setupLongTables(t *testing.T, db *sql.DB) {
	t.Helper()

	for _, ddl := range longTableDropDDL {
		if _, err := db.ExecContext(t.Context(), ddl); err != nil {
			t.Logf("%s: %v (expected on a first run)", ddl, err)
		}
	}

	for _, ddl := range longTableDDL {
		_, err := db.ExecContext(t.Context(), ddl)
		require.NoErrorf(t, err, "setting up the LONG tables: %s", ddl)
	}
}

// logCapturedRows runs one query and logs what it scanned into. Nothing is
// asserted about the values: what a driver turns a column into is its own
// business, and the fixture is about the bytes on the wire.
func logCapturedRows(t *testing.T, db *sql.DB, query string) {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), query)
	require.NoError(t, err)

	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	require.NoError(t, err)
	t.Logf("columns: %v", cols)

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
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
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
