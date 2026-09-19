//go:build capture

// Capture tooling for the REF-cursor fixtures.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 300s -run TestCapture_.*RefCursor -v ./internal/proxy/oracle/
//
// A `SYS_REFCURSOR` handed back by a stored procedure is the one cursor id that
// never rides an OER: the server opens it inside the procedure body while
// executing the call, and reports it in that call's **out-bind** data. These
// recordings are what refCursorIDsInRowData was written against — see
// refcursor_bind_test.go and docs/oracle.md, "Learning a REF cursor's id".
package oracle

import (
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	go_ora "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
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

// ociRefCursorBindOutputFixture is where TestCapture_SQLPlusRefCursor leaves the
// OCI evidence: the call responses, as TNS Data payloads, in the same hex form
// as the other `oci_bundled_*.hex` fixtures.
//
// It is a hex fixture rather than a recording on purpose. `testdata/*.pcapng` is
// a corpus several whole-corpus surveys enumerate, and sqlplus's PL/SQL call
// carries an exec frame the exact statement locator cannot certify — a finding
// of its own, filed separately, and not something to fold into this spec by
// lowering a survey's floor. The bytes this file actually needs are the two
// bind-output responses, so those are what it keeps.
const ociRefCursorBindOutputFixture = "testdata/oci_refcursor_bind_output.hex"

// TestCapture_SQLPlusRefCursor records sqlplus (OCI thick) driving the same
// procedure and writes its call responses to ociRefCursorBindOutputFixture, so
// the fixture set covers the fixed-width encoding too. Skipped when sqlplus is
// not on PATH.
func TestCapture_SQLPlusRefCursor(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := filepath.Join(t.TempDir(), "sqlplus_refcursor.pcapng")

	requireOracleReachable(t, oracleAddr)

	sqlplus, err := exec.LookPath("sqlplus")
	if err != nil {
		t.Skipf("sqlplus unavailable: %v", err)
	}

	w := newCaptureWriter(t, outPath, "capture-sqlplus-refcursor")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

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

	script := writeTempScript(t, body)

	cmd := exec.CommandContext(t.Context(), sqlplus, "-S",
		fmt.Sprintf("system/oracle@//%s/%s", relayAddr, oracleService), "@"+script)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "sqlplus failed: %s", out)
	t.Logf("sqlplus: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	writeBindOutputHexFixture(t, outPath, ociRefCursorBindOutputFixture)
}

// writeBindOutputHexFixture distils a recording down to the server payloads that
// answer a call with an IO vector — the bind-output responses — and writes them
// as one hex line each, the form recordedFrames reads.
func writeBindOutputHexFixture(t *testing.T, dumpPath, outPath string) {
	t.Helper()

	r, err := dump.OpenReader(dumpPath)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	body := "# sqlplus (OCI thick, Instant Client) calling\n" +
		"#   PROCEDURE dbbat_cap_refcur(p OUT SYS_REFCURSOR)\n" +
		"# through dbbat against Oracle 23ai Free. One line per server response that\n" +
		"# opens with the IO vector (TTC message 0x0b): the TNS Data payload, two\n" +
		"# data-flag bytes first, exactly as extractTTCPayload receives it.\n" +
		"#\n" +
		"# These are the same REF cursor descriptors the thin recordings carry, marshaled\n" +
		"# in the wide/fixed-width OCI encoding — four-byte little-endian integers where a\n" +
		"# thin client sends compressed ones. refCursorIDsInBindOutput refuses them at the\n" +
		"# first field rather than reading a number out of the wrong encoding, which is\n" +
		"# what TestOCIRefCursorBindOutputYieldsNoID pins.\n" +
		"#\n" +
		"# Regenerate with:\n" +
		"#   go test -tags capture -run TestCapture_SQLPlusRefCursor ./internal/proxy/oracle/\n"

	frames := 0

	for {
		pkt, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		if pkt.Direction != dump.DirServerToClient {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if ttc := extractTTCPayload(tns.Payload); len(ttc) == 0 || ttc[0] != ttcMsgIOVector {
			continue
		}

		body += hex.EncodeToString(tns.Payload) + "\n"
		frames++
	}

	require.Positive(t, frames, "the sqlplus session must have answered at least one call")
	require.NoError(t, os.WriteFile(outPath, []byte(body), 0o600))

	t.Logf("%d bind-output responses written to %s", frames, outPath)
}
