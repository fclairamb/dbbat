//go:build capture

// Capture tooling for the REF-cursor fixtures.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_.*RefCursor -v ./internal/proxy/oracle/
//
// The sqlplus captures take a second run, because "the OCI encoding" is two
// encodings and each needs its own evidence (see isCloseCursorsWide8Header):
//
//	ORACLE_TEST_OCI_CLIENT=container go test -tags capture -timeout 300s \
//	    -run TestCapture_SQLPlus -v ./internal/proxy/oracle/
//
// which drives the sqlplus bundled in the Oracle image instead of one on PATH.
// Which fixture pair a run writes is decided by the **recorded bytes**, never by
// which side of the container boundary the client came from — the gvenzl images
// bundle different client versions, so the flavor is not the dialect.
//
// A `SYS_REFCURSOR` handed back by a stored procedure is the one cursor id that
// never rides an OER: the server opens it inside the procedure body while
// executing the call, and reports it in that call's **out-bind** data. These
// recordings are what refCursorIDsInBindOutput was written against — see
// refcursor_bind_test.go and docs/oracle.md, "Learning a REF cursor's id".
package oracle

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	go_ora "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/require"
)

// refCursorProcedure is the procedure both captures call. The `OPEN p FOR …`
// inside the body is the statement whose cursor the client then drives, and it
// is deliberately the only thing in there: the recording is about the out-bind,
// not about what the cursor returns.
const refCursorProcedure = `CREATE OR REPLACE PROCEDURE dbbat_cap_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT LEVEL AS n, 'row-' || LEVEL AS label FROM dual CONNECT BY LEVEL <= 5;
END;`

// refCursorDrives is how many times each capture calls the procedure. More than
// one, because the server hands out a **fresh** id per OPEN and the locator has
// to keep up rather than latch onto the first.
const refCursorDrives = 3

// scalarOutBindProcedure is the shape the locator must stay **silent** on, and
// the reason it has a capture of its own: a PL/SQL call with ordinary scalar OUT
// parameters is a bind-output response arriving while a `BEGIN … END;` is in
// flight, which is exactly what learnRefCursorIDs' session gate admits. Nothing
// in it is a cursor, so nothing may be learned from it.
const scalarOutBindProcedure = `CREATE OR REPLACE PROCEDURE dbbat_cap_scalarout(
  n OUT NUMBER, s OUT VARCHAR2, m OUT NUMBER) AS
BEGIN
  n := 7;
  s := 'seven';
  m := 42;
END;`

// TestCapture_GoOraScalarOutBinds records go-ora calling that procedure, three
// times so the recording carries the re-execution shape too.
func TestCapture_GoOraScalarOutBinds(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_SCALAROUT", "testdata/go_ora_scalar_outbinds.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-scalar-outbinds")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	_, err = db.ExecContext(ctx, scalarOutBindProcedure)
	require.NoError(t, err)

	stmt, err := db.PrepareContext(ctx, "BEGIN dbbat_cap_scalarout(:1, :2, :3); END;")
	require.NoError(t, err)

	for i := range refCursorDrives {
		var (
			n, m int64
			s    string
		)

		_, err := stmt.ExecContext(ctx,
			go_ora.Out{Dest: &n}, go_ora.Out{Dest: &s, Size: 32}, go_ora.Out{Dest: &m})
		require.NoErrorf(t, err, "call %d", i+1)
		require.Equal(t, int64(7), n)
		require.Equal(t, "seven", s)
		require.Equal(t, int64(42), m)
	}

	require.NoError(t, stmt.Close())

	_, _ = db.ExecContext(ctx, "DROP PROCEDURE dbbat_cap_scalarout")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d calls)", outPath, refCursorDrives)
}

// pythonScalarOutBindScript is the same shape through python-oracledb thin.
const pythonScalarOutBindScript = `
import sys, oracledb
dsn = sys.argv[1]
calls = int(sys.argv[2])
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    cur = conn.cursor()
    cur.execute("""CREATE OR REPLACE PROCEDURE dbbat_cap_scalarout(
  n OUT NUMBER, s OUT VARCHAR2, m OUT NUMBER) AS
BEGIN
  n := 7;
  s := 'seven';
  m := 42;
END;""")
    for _ in range(calls):
        n = cur.var(oracledb.NUMBER)
        s = cur.var(oracledb.STRING)
        m = cur.var(oracledb.NUMBER)
        cur.callproc("dbbat_cap_scalarout", [n, s, m])
        assert n.getvalue() == 7 and s.getvalue() == "seven" and m.getvalue() == 42
    cur.execute("DROP PROCEDURE dbbat_cap_scalarout")
print("ok")
`

// TestCapture_PythonThinScalarOutBinds records the same call through
// python-oracledb thin, whose out-bind encoding is its own.
func TestCapture_PythonThinScalarOutBinds(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_SCALAROUT_PY", "testdata/python_thin_scalar_outbinds.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-scalar-outbinds")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonScalarOutBindScript)

	cmd := exec.CommandContext(t.Context(), python, script,
		fmt.Sprintf("%s/%s", relayAddr, oracleService), fmt.Sprint(refCursorDrives))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d calls)", outPath, refCursorDrives)
}

// TestCapture_GoOraRefCursor records go-ora calling a procedure with an
// `OUT SYS_REFCURSOR` and then driving the cursor it gets back.
func TestCapture_GoOraRefCursor(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_REFCURSOR", "testdata/go_ora_refcursor.pcapng")

	requireOracleReachable(t, oracleAddr)

	w := newCaptureWriter(t, outPath, "capture-go-ora-refcursor")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	_, err = db.ExecContext(ctx, refCursorProcedure)
	require.NoError(t, err)

	call := "BEGIN dbbat_cap_refcur(:1); END;"

	stmt, err := db.PrepareContext(ctx, call)
	require.NoError(t, err)

	for i := range refCursorDrives {
		var cursor go_ora.RefCursor

		_, err := stmt.ExecContext(ctx, go_ora.Out{Dest: &cursor})
		require.NoErrorf(t, err, "drive %d", i+1)

		ds, err := cursor.Query()
		require.NoError(t, err)

		row := make([]driver.Value, len(ds.Columns()))
		for ds.Next(row) == nil {
			continue
		}

		require.NoError(t, ds.Close())
		require.NoError(t, cursor.Close())
	}

	require.NoError(t, stmt.Close())

	_, _ = db.ExecContext(ctx, "DROP PROCEDURE dbbat_cap_refcur")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d drives)", outPath, refCursorDrives)
}

// pythonRefCursorScript is the same shape through python-oracledb thin, whose
// `cur.var(oracledb.CURSOR)` out-bind is a different client's encoding of the
// same server answer — which is the point of recording both.
const pythonRefCursorScript = `
import sys, oracledb
dsn = sys.argv[1]
drives = int(sys.argv[2])
with oracledb.connect(user="system", password="oracle", dsn=dsn, retry_count=0) as conn:
    cur = conn.cursor()
    cur.execute("""CREATE OR REPLACE PROCEDURE dbbat_cap_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT LEVEL AS n, 'row-' || LEVEL AS label FROM dual CONNECT BY LEVEL <= 5;
END;""")
    for _ in range(drives):
        out = cur.var(oracledb.CURSOR)
        cur.callproc("dbbat_cap_refcur", [out])
        out.getvalue().fetchall()
    cur.execute("DROP PROCEDURE dbbat_cap_refcur")
print("ok")
`

// TestCapture_PythonThinRefCursor records python-oracledb (thin) driving the
// same procedure. Skipped when the interpreter or the driver is not installed —
// this is capture tooling, not CI.
func TestCapture_PythonThinRefCursor(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_REFCURSOR_PY", "testdata/python_thin_refcursor.pcapng")
	python := captureEnv("PYTHON_BIN", "python3")

	requireOracleReachable(t, oracleAddr)

	if out, err := exec.Command(python, "-c", "import oracledb").CombinedOutput(); err != nil {
		t.Skipf("python-oracledb unavailable via %s: %v (%s)", python, err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-python-thin-refcursor")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	script := writeTempScript(t, pythonRefCursorScript)

	cmd := exec.CommandContext(t.Context(), python, script,
		fmt.Sprintf("%s/%s", relayAddr, oracleService), fmt.Sprint(refCursorDrives))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "python client failed: %s", out)
	t.Logf("python client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d drives)", outPath, refCursorDrives)
}

// jdbcRefCursorSource calls the same procedure through the Oracle JDBC thin
// driver, which registers the OUT parameter as `OracleTypes.CURSOR` and gets a
// `ResultSet` back. A third thin client, and the one whose TTC version differs
// most from the other two.
const jdbcRefCursorSource = `
import java.sql.*;
public class RefCur {
  public static void main(String[] a) throws Exception {
    try (Connection c = DriverManager.getConnection("jdbc:oracle:thin:@//" + a[0], "system", "oracle")) {
      try (Statement s = c.createStatement()) {
        s.execute("CREATE OR REPLACE PROCEDURE dbbat_cap_refcur(p OUT SYS_REFCURSOR) AS "
          + "BEGIN OPEN p FOR SELECT LEVEL AS n, 'row-' || LEVEL AS label FROM dual CONNECT BY LEVEL <= 5; END;");
      }
      for (int i = 0; i < Integer.parseInt(a[1]); i++) {
        try (CallableStatement cs = c.prepareCall("BEGIN dbbat_cap_refcur(?); END;")) {
          cs.registerOutParameter(1, oracle.jdbc.OracleTypes.CURSOR);
          cs.execute();
          try (ResultSet rs = (ResultSet) cs.getObject(1)) {
            while (rs.next()) { rs.getInt(1); }
          }
        }
      }
      try (Statement s = c.createStatement()) { s.execute("DROP PROCEDURE dbbat_cap_refcur"); }
    }
    System.out.println("ok");
  }
}
`

// TestCapture_JDBCThinRefCursor records the Oracle JDBC thin driver driving the
// same procedure. Skipped when no JDK or ojdbc jar is around; point OJDBC_JAR at
// one to override the default (the jar SQLcl ships).
func TestCapture_JDBCThinRefCursor(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_REFCURSOR_JDBC", "testdata/jdbc_thin_refcursor.pcapng")
	jar := captureEnv("OJDBC_JAR", "/opt/homebrew/Caskroom/sqlcl/26.1.0.086.1709/sqlcl/lib/ojdbc11.jar")

	requireOracleReachable(t, oracleAddr)

	if _, err := os.Stat(jar); err != nil {
		t.Skipf("ojdbc jar not found at %s: %v", jar, err)
	}

	if _, err := exec.LookPath("javac"); err != nil {
		t.Skipf("javac unavailable: %v", err)
	}

	dir := t.TempDir()
	src := dir + "/RefCur.java"
	require.NoError(t, os.WriteFile(src, []byte(jdbcRefCursorSource), 0o600))

	if out, err := exec.Command("javac", "-cp", jar, "-d", dir, src).CombinedOutput(); err != nil {
		t.Skipf("javac failed: %v (%s)", err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-jdbc-thin-refcursor")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	cmd := exec.CommandContext(t.Context(), "java", "-cp", dir+":"+jar, "RefCur",
		fmt.Sprintf("%s/%s", relayAddr, oracleService), fmt.Sprint(refCursorDrives))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "jdbc client failed: %s", out)
	t.Logf("jdbc client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d drives)", outPath, refCursorDrives)
}

// The OCI evidence is kept as hex fixtures rather than as recordings on
// purpose. `testdata/*.pcapng` is a corpus several whole-corpus surveys
// enumerate, and sqlplus's PL/SQL call carries an exec frame the exact
// statement locator cannot certify — a finding of its own, filed separately,
// and not something to fold in by lowering a survey's floor. The bytes these
// tests actually need are the bind-output responses and the client frame that
// follows each, so those are what is kept. The paths themselves, and why there
// are two sets of them, are in oci_fixture_capture_test.go.

// ociCaptureContainerEnv names the running Oracle container a `container`
// capture execs into — the one the file header tells you to start.
const ociCaptureContainerEnv = "ORACLE_CONTAINER"

// ociCaptureHostGateway is the name that container resolves back to the Docker
// host, which is where the capture relay listens.
const ociCaptureHostGateway = "host.docker.internal"

// ociCaptureScriptPath is where a script is dropped inside that container. /tmp
// is writable by the `oracle` user the image runs as.
const ociCaptureScriptPath = "/tmp/dbbat_cap_oci.sql"

// ociCaptureClient is an sqlplus a capture can drive, wherever it lives.
//
// bindHost is the address the relay must listen on for this client to reach it,
// and dialHost is what the client puts in its connect string. They differ for
// the container-hosted one and only for it.
type ociCaptureClient struct {
	label    string
	bindHost string
	dialHost string
	run      func(t *testing.T, connect, script string) (string, error)
}

// sqlplusCaptureClient picks where sqlplus comes from, honouring the same
// variable the live suite uses (ociClientEnv in oci_client_integration_test.go)
// so "run the capture the way CI runs the tests" is one spelling, not two.
// Unset prefers a client on PATH, exactly as plannedOCIClient does.
func sqlplusCaptureClient(t *testing.T) *ociCaptureClient {
	t.Helper()

	want := os.Getenv("ORACLE_TEST_OCI_CLIENT")

	if want != "container" {
		sqlplus, err := exec.LookPath("sqlplus")
		if err == nil {
			return hostSQLPlusCaptureClient(sqlplus)
		}

		if want == "path" {
			t.Skipf("sqlplus unavailable on PATH: %v", err)
		}
	}

	return containerSQLPlusCaptureClient(t)
}

// hostSQLPlusCaptureClient wraps an sqlplus found on PATH.
func hostSQLPlusCaptureClient(sqlplus string) *ociCaptureClient {
	return &ociCaptureClient{
		label:    "sqlplus on PATH (" + sqlplus + ")",
		bindHost: "127.0.0.1",
		dialHost: "127.0.0.1",
		run: func(t *testing.T, connect, script string) (string, error) {
			t.Helper()

			path := writeTempScript(t, script)

			out, err := exec.CommandContext(t.Context(), sqlplus, "-S", connect, "@"+path).CombinedOutput()

			return string(out), err
		},
	}
}

// containerSQLPlusCaptureClient wraps the sqlplus bundled in the running Oracle
// container, reached over `docker exec` and dialing the relay back out over the
// host gateway.
//
// Both facts are probed before the capture starts, because each is an
// environment fact rather than a finding: that the image bundles a usable
// sqlplus at all, and that the container can open a TCP connection back to the
// host. Letting either through would land as a bewildering TNS error in the
// middle of a recording.
func containerSQLPlusCaptureClient(t *testing.T) *ociCaptureClient {
	t.Helper()

	container := captureEnv(ociCaptureContainerEnv, "dbbat-ora-cap")

	if out, err := exec.Command("docker", "exec", container, "sqlplus", "-v").CombinedOutput(); err != nil {
		t.Skipf("no usable sqlplus in container %s: %v (%s)", container, err, out)
	}

	probe := "exec 3<>/dev/tcp/" + ociCaptureHostGateway + "/1"
	if out, err := exec.Command("docker", "exec", container, "bash", "-c",
		"getent hosts "+ociCaptureHostGateway+" >/dev/null || "+probe).CombinedOutput(); err != nil {
		t.Skipf("container %s cannot resolve %s: %v (%s)", container, ociCaptureHostGateway, err, out)
	}

	return &ociCaptureClient{
		label:    "sqlplus bundled in container " + container,
		bindHost: "0.0.0.0",
		dialHost: ociCaptureHostGateway,
		run: func(t *testing.T, connect, script string) (string, error) {
			t.Helper()

			// Written through the container's own shell rather than `docker cp`,
			// which lands the file owned by root while sqlplus runs as `oracle`
			// and then reports SP2-0310 "unable to open file" — a failure that
			// reads as a client problem and is not one.
			write := exec.CommandContext(t.Context(), "docker", "exec", "-i", container,
				"bash", "-c", "cat > "+ociCaptureScriptPath)
			write.Stdin = strings.NewReader(script)

			if out, err := write.CombinedOutput(); err != nil {
				return string(out), err
			}

			out, err := exec.CommandContext(t.Context(), "docker", "exec", container,
				"sqlplus", "-S", connect, "@"+ociCaptureScriptPath).CombinedOutput()

			return string(out), err
		},
	}
}

// runSQLPlusCapture records one sqlplus script end to end and returns the
// recording's path.
//
// The relay's port is what the client dials; its *address* is not, because a
// wildcard-bound listener reports 0.0.0.0 and no client can dial that.
func runSQLPlusCapture(t *testing.T, client *ociCaptureClient, sessionID, script string) string {
	t.Helper()

	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")

	// The recording itself is scratch — only the hex fixtures distilled out of it
	// are kept — but CAPTURE_KEEP_DUMP_DIR leaves it somewhere durable, which is
	// what you want the first time a client turns out to speak a dialect the
	// distillation does not expect.
	outPath := filepath.Join(captureEnv("CAPTURE_KEEP_DUMP_DIR", t.TempDir()), sessionID+".pcapng")

	requireOracleReachable(t, oracleAddr)

	t.Logf("OCI capture client: %s", client.label)

	w := newCaptureWriter(t, outPath, sessionID)
	relayAddr := startCaptureRelayOn(t, client.bindHost, oracleAddr, w)

	_, port, err := net.SplitHostPort(relayAddr)
	require.NoError(t, err)

	connect := fmt.Sprintf("system/oracle@//%s:%s/%s", client.dialHost, port, oracleService)

	out, err := client.run(t, connect, script)
	require.NoErrorf(t, err, "sqlplus failed: %s", out)
	t.Logf("sqlplus: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	return outPath
}

// TestCapture_SQLPlusRefCursor records sqlplus (OCI thick) driving the same
// procedure and writes its call responses to the fixture pair of whichever OCI
// dialect the recording turns out to hold, so the fixture set covers both
// fixed-width encodings. Skipped when no sqlplus can be reached.
func TestCapture_SQLPlusRefCursor(t *testing.T) {
	client := sqlplusCaptureClient(t)

	body := refCursorProcedure + `
/
VARIABLE rc REFCURSOR
BEGIN dbbat_cap_refcur(:rc); END;
/
PRINT rc
BEGIN dbbat_cap_refcur(:rc); END;
/
PRINT rc
DROP PROCEDURE dbbat_cap_refcur;
EXIT
`

	outPath := runSQLPlusCapture(t, client, "capture-sqlplus-refcursor", body)

	bindOutputs, drives := ociRefCursorBindOutputFixture, ociRefCursorDrivesFixture
	if recordedDialectIsWide64(t, outPath) {
		bindOutputs, drives = oci64RefCursorBindOutputFixture, oci64RefCursorDrivesFixture
	}

	writeBindOutputHexFixture(t, outPath, bindOutputs, drives)
}

// TestCapture_SQLPlusScalarOutBinds records sqlplus calling a procedure with
// ordinary scalar OUT parameters, and keeps its call responses as the OCI
// counterpart of testdata/go_ora_scalar_outbinds.pcapng. Nothing in it is a
// cursor, so nothing may be learned from it —
// TestOCIScalarOutBindsYieldNoRefCursorID.
func TestCapture_SQLPlusScalarOutBinds(t *testing.T) {
	client := sqlplusCaptureClient(t)

	body := scalarOutBindProcedure + `
/
VARIABLE n NUMBER
VARIABLE s VARCHAR2(32)
VARIABLE m NUMBER
BEGIN dbbat_cap_scalarout(:n, :s, :m); END;
/
BEGIN dbbat_cap_scalarout(:n, :s, :m); END;
/
PRINT n
DROP PROCEDURE dbbat_cap_scalarout;
EXIT
`

	outPath := runSQLPlusCapture(t, client, "capture-sqlplus-scalar-outbinds", body)

	fixture := ociScalarOutBindFixture
	if recordedDialectIsWide64(t, outPath) {
		fixture = oci64ScalarOutBindFixture
	}

	writeBindOutputHexFixture(t, outPath, fixture, "")
}
