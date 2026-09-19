//go:build capture

// Capture tooling for the oldest Oracle client that can still be obtained.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	curl -O https://repo1.maven.org/maven2/com/oracle/database/jdbc/ojdbc6/11.2.0.4/ojdbc6-11.2.0.4.jar
//	OJDBC6_JAR=$PWD/ojdbc6-11.2.0.4.jar \
//	  go test -tags capture -timeout 300s -run TestCapture_LegacyOALL8 -v ./internal/proxy/oracle/
//
// It exists to answer one question: **does a pre-v315 client send the legacy
// `OALL8` op (func 0x0E) dbbat models in `decodeOALL8` and refuses to rewrite?**
// ojdbc6 11.2.0.4 is the oldest Oracle driver still reachable from a package
// repository (Maven Central, `com.oracle.database.jdbc:ojdbc6`; the `ojdbc14`
// 10.2 coordinate on Central is a 554-byte licence stub, not a driver), and
// Oracle 23ai Free still accepts it.
//
// Measured on 2026-09-19, and the answer is no:
//
//   - the session negotiates **TNS version 310** (ACCEPT payload `01 36`), i.e.
//     genuinely pre-v315, and
//   - every statement it sends is still the piggyback exec — `03 5e`, plus the
//     `11 69`-stapled twin — which the rewriter already covers
//     (`compressed/bare`). Not one frame starts with 0x0E.
//
// Oracle's own driver agrees: `oracle.jdbc.driver.T4C8Oall` in that same jar —
// the class named after OALL8 — builds a `T4CTTIfun` of message type 3 with
// function code 94 (**0x5E**). So "OALL8" in Oracle's own vocabulary *is* the
// `03 5e` frame dbbat already tags, and nothing observed anywhere emits a
// statement frame whose first byte is 0x0E.
//
// The recording is deliberately **not** a corpus fixture, which is why the
// default output path is a temporary file rather than `testdata/`: the session
// also carries an ojdbc6 prepared-statement re-execution sent as `03 5e` with no
// statement text, which `frameCarriesStatement` counts and the locator (rightly)
// refuses — so dropping the file into `testdata/` trips the coverage floor in
// `TestSurveyStatementRewriteCorpus`. That frame is its own finding; see
// `specs/todos/2026-09-19-02-oracle-piggyback-exec-5e-reexec-ungated.md`.
package oracle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// legacyOALL8Source drives the shapes a statement rewriter has to survive:
// a short statement, one long enough to cross the 252-byte CLR short-form limit,
// and a bound statement executed twice (the second execution being the SQL-less
// re-execution this client sends under sub-op 0x5e).
const legacyOALL8Source = `
import java.sql.*;

public class LegacyOALL8 {
    public static void main(String[] a) throws Exception {
        Class.forName("oracle.jdbc.OracleDriver");
        try (Connection c = DriverManager.getConnection("jdbc:oracle:thin:@//" + a[0], "system", "oracle")) {
            System.out.println("driver=" + c.getMetaData().getDriverVersion());

            try (Statement s = c.createStatement();
                 ResultSet r = s.executeQuery("SELECT 1 AS n FROM dual")) {
                while (r.next()) System.out.println("short=" + r.getInt(1));
            }

            StringBuilder pad = new StringBuilder();
            while (pad.length() < 300) pad.append("padpadpad ");
            String longSQL = "SELECT 2 AS n FROM dual WHERE '" + pad + "' IS NOT NULL";
            try (Statement s = c.createStatement();
                 ResultSet r = s.executeQuery(longSQL)) {
                while (r.next()) System.out.println("long=" + r.getInt(1));
            }

            try (PreparedStatement p = c.prepareStatement("SELECT ? AS n FROM dual")) {
                for (int i = 3; i < 5; i++) {
                    p.setInt(1, i);
                    try (ResultSet r = p.executeQuery()) {
                        while (r.next()) System.out.println("bound=" + r.getInt(1));
                    }
                }
            }
        }
        System.out.println("ok");
    }
}
`

// TestCapture_LegacyOALL8 records an ojdbc6 (11.2.0.4) session against Oracle
// 23ai Free. Skipped when the jar or a JDK is missing — this is capture tooling,
// not CI. Analyse the result with:
//
//	DUMP_PATH=<the path it logs> go test -tags capture -run TestAnalyzeDump -v ./internal/proxy/oracle/
func TestCapture_LegacyOALL8(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LEGACY_OALL8",
		filepath.Join(os.TempDir(), "ojdbc6_legacy.pcapng"))

	jar := captureEnv("OJDBC6_JAR", "")
	if jar == "" {
		t.Skip("OJDBC6_JAR unset: point it at com.oracle.database.jdbc:ojdbc6:11.2.0.4")
	}

	if _, err := os.Stat(jar); err != nil {
		t.Skipf("ojdbc6 jar not found at %s: %v", jar, err)
	}

	if _, err := exec.LookPath("javac"); err != nil {
		t.Skipf("javac unavailable: %v", err)
	}

	requireOracleReachable(t, oracleAddr)

	dir := t.TempDir()
	src := dir + "/LegacyOALL8.java"
	require.NoError(t, os.WriteFile(src, []byte(legacyOALL8Source), 0o600))

	if out, err := exec.Command("javac", "-nowarn", "-d", dir, src).CombinedOutput(); err != nil {
		t.Skipf("javac failed: %v (%s)", err, out)
	}

	w := newCaptureWriter(t, outPath, "capture-ojdbc6-legacy-oall8")
	relayAddr := startCaptureRelay(t, oracleAddr, w)

	cmd := exec.CommandContext(t.Context(), "java", "-cp", dir+":"+jar, "LegacyOALL8",
		fmt.Sprintf("%s/%s", relayAddr, oracleService))

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "ojdbc6 client failed: %s", out)
	t.Logf("ojdbc6 client: %s", out)

	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}
