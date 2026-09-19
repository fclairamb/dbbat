//go:build integration

package oracle

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// The oldest Oracle client dbbat can be pointed at, driven through the real
// proxy.
//
// ojdbc6 11.2.0.4 is the pre-v315 shape — TNS version 310 — and until
// 2026-09-19 it could not log in at all: the AUTH leg framed every packet it
// built in the v315+ form and read three *server* capabilities (customHash,
// UseBigClrChunks, and by extension the 12c verifier) as if they were the
// session's. Four separate walls, each hidden behind the one in front of it;
// see docs/oracle.md, "Pre-v315 clients".
//
// That makes this file the live half of a corpus that was, until the login
// worked, replay-only: `testdata/ojdbc6_legacy.pcapng` was recorded through a
// dumb byte relay (capture_legacy_oall8_test.go), so every claim about how
// dbbat treats this client's frames — the `03 5e` re-execution gate above all —
// could be checked against the recorded bytes but never against a running
// session. It can now.
//
// The jar is an explicit path, the same rule ojdbcJarEnv follows for the modern
// driver: there is no packaged Oracle JDBC driver to look up, and this one is a
// specific 2013 artifact rather than whatever ojdbc happens to be around.
//
//	curl -O https://repo1.maven.org/maven2/com/oracle/database/jdbc/ojdbc6/11.2.0.4/ojdbc6-11.2.0.4.jar
//	OJDBC6_JAR=$PWD/ojdbc6-11.2.0.4.jar make test-e2e-oracle
const ojdbc6JarEnv = "OJDBC6_JAR"

// oracleTestOJDBC6Jar resolves the ojdbc6 jar, or "" when this machine has
// none. A variable pointing at a file that is not there is a failure rather
// than a skip: it is a typo, not an absence.
func oracleTestOJDBC6Jar(t *testing.T) string {
	t.Helper()

	jar := os.Getenv(ojdbc6JarEnv)
	if jar == "" {
		return ""
	}

	_, err := os.Stat(jar)
	require.NoErrorf(t, err, "%s points at %s, which cannot be read", ojdbc6JarEnv, jar)

	return jar
}

// requireOJDBC6 resolves the JDK and the ojdbc6 jar, skipping when either is
// missing — the same rule requireOJDBC applies to the modern driver.
func requireOJDBC6(t *testing.T) (string, string) {
	t.Helper()

	jar := oracleTestOJDBC6Jar(t)
	if jar == "" {
		t.Skipf("no ojdbc6 driver: set %s to com.oracle.database.jdbc:ojdbc6:11.2.0.4", ojdbc6JarEnv)
	}

	java, err := exec.LookPath("java")
	require.NoErrorf(t, err, "%s is set but there is no java on PATH to run the probe with", ojdbc6JarEnv)

	return java, jar
}

// ojdbc6ProbeProgram drives the two claims from the client side. Run through
// `java -cp <jar> Probe.java` (single-file source mode), so no build tooling
// beyond a JDK is involved and the file must be named Probe.java.
//
// Both modes prepare `SELECT ? AS n FROM dual` and execute it more than once.
// That is the only way to make ojdbc6 emit the frame this file exists for: its
// second execution goes out as `03 5e` — the *parse* op — with the statement
// length set to zero, naming the cursor the first execution created and
// carrying no SQL text at all.
//
// Modes:
//
//	reexec  connect, run one plain statement, then execute a bound
//	        PreparedStatement twice. The happy path, and the re-execution the
//	        gate has to see.
//	quota   execute the same PreparedStatement in a loop until dbbat refuses
//	        one, printing the run it died on. The loop is what makes the
//	        measurement about a *re-execution* being refused: run 1 is the
//	        parse, so a refusal at run 2 or later cannot be the parse.
//
// getMessage() is split on the first line because the driver appends a
// connection id the assertions do not care about.
const ojdbc6ProbeProgram = `import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;

public class Probe {
    public static void main(String[] args) throws Exception {
        Class.forName("oracle.jdbc.OracleDriver");

        String url = String.format("jdbc:oracle:thin:@//%s:%s/%s", args[0], args[1], args[2]);
        String mode = args[5];

        try (Connection conn = DriverManager.getConnection(url, args[3], args[4])) {
            System.out.println("driver=" + conn.getMetaData().getDriverVersion());

            if (mode.equals("reexec")) {
                try (Statement st = conn.createStatement();
                     ResultSet rs = st.executeQuery("SELECT 7 FROM dual")) {
                    rs.next();
                    System.out.println("plain=" + rs.getInt(1));
                }

                try (PreparedStatement ps = conn.prepareStatement("SELECT ? AS n FROM dual")) {
                    for (int i = 1; i <= 2; i++) {
                        ps.setInt(1, i);
                        try (ResultSet rs = ps.executeQuery()) {
                            rs.next();
                            System.out.println("bound=" + rs.getInt(1));
                        }
                    }
                }
            } else {
                try (PreparedStatement ps = conn.prepareStatement("SELECT ? AS n FROM dual")) {
                    for (int i = 1; i <= 8; i++) {
                        ps.setInt(1, i);
                        try (ResultSet rs = ps.executeQuery()) {
                            rs.next();
                            System.out.println("ran=" + rs.getInt(1));
                        } catch (SQLException e) {
                            System.out.println("refused-at=" + i + " " + e.getMessage().trim().split("\\R")[0]);
                            break;
                        }
                    }
                } catch (SQLException e) {
                    System.out.println("refused-at=outer " + e.getMessage().trim().split("\\R")[0]);
                }
            }
        }

        System.out.println("done");
    }
}
`

// ojdbc6ProbeDeadline is a deadlock detector with a JVM's startup subtracted
// from it, the same shape as jdbcRefusalDeadline.
const ojdbc6ProbeDeadline = 3 * time.Minute

// runOJDBC6Probe runs the program above in one mode and returns everything it
// printed.
func runOJDBC6Probe(t *testing.T, env *oracleThroughProxy, java, jar, program, mode string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ojdbc6ProbeDeadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, java, "-cp", jar, program,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey, mode)

	out, err := cmd.CombinedOutput()
	output := string(out)

	// The connect itself is the thing under test in the first subtest, so a
	// failure has to print what the driver said rather than "exit status 1".
	require.NoErrorf(t, err, "ojdbc6 probe (%s) failed:\n%s", mode, output)

	return output
}

// refusedAtRun reads the run index out of a `refused-at=N ...` line.
var refusedAtRun = regexp.MustCompile(`refused-at=(\d+)`)

// TestIntegration_OJDBC6ThroughTheProxy is the whole pre-v315 story in one
// fixture: a client that could not log in at all now logs in, runs statements,
// and has its cursor re-executions gated like anyone else's.
//
// One fixture, two subtests, because an Oracle container start costs minutes
// and the fixture can swap the grant between them — each subtest opens its own
// connection and a session resolves its grant once, at auth.
func TestIntegration_OJDBC6ThroughTheProxy(t *testing.T) {
	java, jar := requireOJDBC6(t)

	env := startOracleThroughProxy(t, nil)

	program := writeOJDBC6Probe(t)

	t.Run("logs in and executes", func(t *testing.T) {
		output := runOJDBC6Probe(t, env, java, jar, program, "reexec")

		// The spec's actual goal, and the line that would have been an
		// `Invalid Packet Lenght` stack trace before the AUTH leg learned the
		// pre-v315 framing.
		assert.Contains(t, output, "driver=11.2.0.4",
			"the probe must be the 2013 driver, not some other jar on the classpath:\n%s", output)
		assert.Contains(t, output, "plain=7", "a plain statement must come back:\n%s", output)
		assert.Contains(t, output, "bound=1", "the prepared statement's first execution:\n%s", output)
		assert.Contains(t, output, "bound=2", "and its second, which is the re-execution:\n%s", output)
		assert.Contains(t, output, "done", "the session must close cleanly:\n%s", output)

		// The live counterpart of TestDumpReplay_OJDBC6ReexecIsGatedOnRealFrames.
		// The replay could show the gate *recognizing* the recorded frame; only a
		// real session can show the frame arriving from a real driver and the
		// statement it resolves to being written down.
		assert.Positive(t, env.logs.count(logMsgReexecGated),
			"the SQL-less 03 5e ojdbc6 sends must reach the re-execution gate")
		assert.Zero(t, env.logs.count(logMsgUntrackedCursorForwarded),
			"its cursor was parsed in this very session, so it must resolve rather than be waved through")

		for _, sql := range env.logs.sqlsFor(logMsgReexecGated) {
			assert.Contains(t, sql, "FROM dual",
				"a re-executed cursor must resolve to the statement it was parsed with")
		}

		// And the row: before the gate existed, every execution after the first
		// ran with no `queries` row at all.
		rows := env.boundProbeQueries(t, 2)
		assert.GreaterOrEqual(t, len(rows), 2,
			"each execution of the prepared statement is a query of its own")

		for i := range rows {
			assert.Nil(t, rows[i].Error, "neither execution is refused under a full-write grant")
		}
	})

	t.Run("a re-execution is refused by an exhausted quota", func(t *testing.T) {
		// A *query-count* quota rather than the byte quota: the byte budget is
		// owned by the limit watchdog, which tears the session down from its own
		// goroutine within one poll interval of the budget being crossed (see
		// LimitGuard.Watch, and TestIntegration_AsyncRefusalAgainstOCIAndPythonThin
		// for that path end to end). Racing a 250ms timer against the next
		// round trip would make this measurement a coin toss. The query count is
		// checked synchronously by checkQuotas, in the same book() step the
		// re-execution's own controls run in — which is precisely the step this
		// test is about.
		env.replaceGrant(t, nil, testsupport.WithMaxQueryCounts(ojdbc6QuotaRuns))

		output := runOJDBC6Probe(t, env, java, jar, program, "quota")

		require.Contains(t, output, "refused-at=",
			"the quota must stop the loop; eight executions under a budget of %d cannot all pass:\n%s",
			ojdbc6QuotaRuns, output)

		match := refusedAtRun.FindStringSubmatch(output)
		require.Lenf(t, match, 2, "the refusal must name the run it happened on:\n%s", output)

		run, err := strconv.Atoi(match[1])
		require.NoError(t, err)

		assert.Greaterf(t, run, 1,
			"run 1 is the parse; a refusal there would say nothing about re-executions:\n%s", output)
		assert.Contains(t, output, "ran=1",
			"the first execution must succeed, so the quota is exhausted *by* it:\n%s", output)
		assert.Contains(t, output, "ORA-",
			"the refusal must reach the driver as an ORA error, not a dead socket:\n%s", output)

		// The refusal is written down like any other, which is the half a client
		// message cannot prove. Waited on rather than counted once: the rows from
		// the subtest above are already there, so "at least one row exists" would
		// pass before this subtest's refusal had been persisted at all.
		row := env.awaitRefusedProbeQuery(t)
		assert.Contains(t, *row.Error, "limit",
			"the row must carry the quota as its reason, not some other refusal")
		assert.Contains(t, row.SQLText, "FROM dual",
			"and the statement the re-executed cursor was parsed with, which is the only "+
				"place this execution's text exists")
	})
}

// ojdbc6QuotaRuns is the query budget the quota subtest issues. Small enough
// that the probe's eight executions cannot all fit, and large enough that the
// parse and at least one execution do — which is what makes the refusal land on
// a re-execution rather than on the statement that created the cursor.
const ojdbc6QuotaRuns = 3

// writeOJDBC6Probe drops the probe program somewhere java's source mode can run
// it from. The file name has to be Probe.java.
func writeOJDBC6Probe(t *testing.T) string {
	t.Helper()

	path := t.TempDir() + "/Probe.java"
	require.NoError(t, os.WriteFile(path, []byte(ojdbc6ProbeProgram), 0o600))

	return path
}

// awaitRefusedProbeQuery waits for a refusal row on the probe's prepared
// statement and returns it.
func (e *oracleThroughProxy) awaitRefusedProbeQuery(t *testing.T) store.Query {
	t.Helper()

	ctx := context.Background()

	var refused store.Query

	require.Eventually(t, func() bool {
		queries, err := e.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		for i := range queries {
			if !strings.Contains(queries[i].SQLText, "AS n FROM dual") {
				continue
			}

			if queries[i].Error != nil && *queries[i].Error != "" {
				refused = queries[i]

				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond,
		"the refused re-execution was never logged as a query of its own")

	return refused
}

// boundProbeQueries returns the `queries` rows the probe's prepared statement
// produced, waiting until at least want of them are there.
//
// The match is on the statement's tail rather than on ojdbc6ProbeSQL: the
// driver rewrites `?` to `:1` before it ever reaches the wire, and what dbbat
// records is the text the *client* sent.
func (e *oracleThroughProxy) boundProbeQueries(t *testing.T, want int) []store.Query {
	t.Helper()

	ctx := context.Background()

	var matches []store.Query

	require.Eventuallyf(t, func() bool {
		queries, err := e.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		matches = nil

		for i := range queries {
			if strings.Contains(queries[i].SQLText, "AS n FROM dual") {
				matches = append(matches, queries[i])
			}
		}

		return len(matches) >= want
	}, 30*time.Second, 250*time.Millisecond,
		"fewer than %d rows for the probe's prepared statement were ever logged", want)

	return matches
}
