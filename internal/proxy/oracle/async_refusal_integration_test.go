//go:build integration

package oracle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
)

// asyncProbeProgram is the JDBC half of the asynchronous-refusal measurement.
// One program, three modes, because all three want the same connect, the same
// error reporting and the same "never throw out of main" discipline — a probe
// that exits non-zero tells the Go side nothing about *why*.
//
// It reports an error the way this measurement needs it read: the vendor code
// and SQLState, then the whole cause chain. That chain is the entire point.
// ojdbc 26.1 does not swallow an error whose call number it disagrees with — it
// reports ORA-18745 "Execution error in sessionless transaction piggybacked
// call" and demotes the real ORA-NNNNN to a *cause*, so an assertion that only
// looked for "ORA-00028 appears somewhere" would pass on the failure mode this
// is here to detect.
//
// Modes:
//
//	quota   — drain a large result set with a bounded fetch size, so dbbat's
//	          byte quota trips in the middle of a *fetch* reply rather than at
//	          a statement boundary.
//	idle    — announce readiness, block on stdin while the Go side does
//	          something to the grant, then run one statement.
//	session — idle, plus its own v$session sid/serial# first, which is what the
//	          real-Oracle capture needs to kill it from another connection.
//
// Run through `java AsyncProbe.java` (single-file source mode), so the file
// must be named AsyncProbe.java.
const asyncProbeProgram = `import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;

public class AsyncProbe {
    public static void main(String[] args) {
        String url = String.format("jdbc:oracle:thin:@//%s:%s/%s", args[0], args[1], args[2]);
        String mode = args[5];

        Connection conn = null;

        try {
            conn = DriverManager.getConnection(url, args[3], args[4]);

            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("SELECT 1 FROM dual")) {
                rs.next();
                System.out.println("read-ok " + rs.getInt(1));
            }

            if (mode.equals("quota")) {
                quota(conn, Integer.parseInt(args[6]));
            } else {
                if (mode.equals("session")) {
                    try (Statement st = conn.createStatement();
                         ResultSet rs = st.executeQuery(
                             "SELECT sid, serial# FROM v$session WHERE sid = SYS_CONTEXT('USERENV','SID')")) {
                        rs.next();
                        System.out.println("session " + rs.getInt(1) + " " + rs.getInt(2));
                    }
                }

                idle(conn);
            }
        } catch (SQLException e) {
            report("connect", e);
        } catch (Exception e) {
            System.out.println("unexpected: " + e);
        } finally {
            try {
                if (conn != null) {
                    conn.close();
                }
            } catch (Exception ignored) {
                // A session dbbat already tore down cannot be closed politely,
                // and that is not what is being measured.
            }
        }

        System.out.println("done");
        System.out.flush();
    }

    // quota drains a result set big enough that dbbat's byte quota is crossed
    // while rows are still streaming. The row count printed before the error is
    // what says the refusal landed mid-result-set rather than at admission.
    static void quota(Connection conn, int fetchSize) {
        int rows = 0;

        try (Statement st = conn.createStatement()) {
            st.setFetchSize(fetchSize);

            try (ResultSet rs = st.executeQuery("SELECT id, payload FROM dbbat_quota_probe ORDER BY id")) {
                while (rs.next()) {
                    rows++;
                }
            }

            System.out.println("QUOTA-NOT-TRIPPED rows=" + rows);
        } catch (SQLException e) {
            System.out.println("rows-before-error " + rows);
            report("midfetch", e);
        }
    }

    // idle parks the client between calls — no statement in flight, nothing in
    // the receive buffer — and only then runs the statement whose answer is the
    // measurement.
    static void idle(Connection conn) {
        try {
            System.out.println("idle-ready");
            System.out.flush();

            new BufferedReader(new InputStreamReader(System.in)).readLine();

            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("SELECT 42 FROM dual")) {
                rs.next();
                System.out.println("NO-INTERRUPTION " + rs.getInt(1));
            }
        } catch (SQLException e) {
            report("after-idle", e);
        } catch (Exception e) {
            System.out.println("unexpected: " + e);
        }
    }

    static void report(String label, SQLException e) {
        System.out.println(label + ": code=" + e.getErrorCode()
            + " state=" + e.getSQLState()
            + " class=" + e.getClass().getName()
            + " msg=" + firstLine(e.getMessage()));

        Throwable cause = e.getCause();

        for (int depth = 0; cause != null && depth < 5; depth++) {
            System.out.println(label + "-cause: " + cause.getClass().getName()
                + ": " + firstLine(String.valueOf(cause.getMessage())));
            cause = cause.getCause();
        }
    }

    static String firstLine(String s) {
        return s == null ? "" : s.trim().split("\\R")[0];
    }
}
`

// quotaProbeRows / quotaProbeWidth size the result set the byte quota is tripped
// in, and quotaProbeBudget is the quota itself. The numbers matter to each
// other, not on their own: the payload has to outweigh the budget several times
// over so the crossing lands well inside the stream, and the budget has to
// outweigh a JDBC connect (tens of kilobytes) so the statement is *admitted*
// and cut afterwards rather than refused at the gate.
const (
	quotaProbeRows   = 5000
	quotaProbeWidth  = 400
	quotaProbeFetch  = 500
	quotaProbeBudget = 600_000
)

// idleRevokeSettle is how long the Go side waits after revoking, before letting
// the client speak again. The watchdog polls at shared.DefaultLimitPollInterval
// (250ms); this is an order of magnitude more, so a client that still gets an
// answer is a real finding and not a race lost.
const idleRevokeSettle = 3 * time.Second

// TestIntegration_AsyncRefusalAgainstJDBCThin is the measurement behind
// docs/oracle.md, "An asynchronous refusal: which call number, and whether to
// send one at all".
//
// TestIntegration_BlockedStatementRefusesJDBCThin measures a refusal that
// *answers* a statement. The two cases here are the ones that do not:
//
//  1. a byte quota crossed while dbbat is relaying a reply — a refusal cut into
//     a call the client is still parked in the receive for. dbbat writes an
//     ORA-00028 stamped with the last call number it observed, and the question
//     the measurement settles is whether ojdbc 26.1 accepts that number or
//     mislabels the error as ORA-18745;
//  2. a grant revoked while the client sits idle between calls — the one path
//     where there is no call to end, where dbbat deliberately writes no frame
//     at all and force-closes both sockets instead.
//
// Both run on ojdbc 26.1 for a reason: 23.2 never checks the sequence number,
// so it would pass either way and prove nothing.
//
// One fixture, two subtests, because an Oracle container start costs minutes
// and the fixture can swap the grant between them (each subtest opens its own
// connection, which is what picks the new grant up).
func TestIntegration_AsyncRefusalAgainstJDBCThin(t *testing.T) {
	java, jar := requireOJDBC(t)

	env := startOracleThroughProxy(t, nil)
	ctx := context.Background()

	// Seeded under the permissive grant the fixture starts with, before any
	// quota exists: the seed itself would otherwise consume the budget it is
	// there to overflow.
	_, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_quota_probe")

	_, err := env.db.ExecContext(ctx,
		fmt.Sprintf("CREATE TABLE dbbat_quota_probe (id NUMBER, payload VARCHAR2(%d))", quotaProbeWidth))
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	// DBMS_RANDOM rather than a constant, and that is not decoration: Oracle
	// compresses a column whose value is unchanged from the previous row out of
	// the wire entirely, so a table of 5000 identical payloads streams as a few
	// kilobytes and no byte quota worth the name is ever crossed. Measured: with
	// RPAD('x', 400, 'x') the probe drained all 5000 rows under a 600 KB budget.
	_, err = env.db.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO dbbat_quota_probe SELECT level, DBMS_RANDOM.STRING('x', %d) FROM dual CONNECT BY level <= %d",
		quotaProbeWidth, quotaProbeRows))
	require.NoError(t, err, "seeding the result set the quota is tripped in")

	program := filepath.Join(t.TempDir(), "AsyncProbe.java")
	require.NoError(t, os.WriteFile(program, []byte(asyncProbeProgram), 0o600))

	// Both probes dial dbbat through a recording tap. "Did dbbat write a frame
	// at all?" is the question both cases turn on, and a driver reporting
	// "connection closed by peer" cannot answer it — a frame never written and
	// a frame written and lost look identical from there.
	tap := startRecordingTap(t, env.host, env.port)

	probe := jdbcProbeRun{
		java: java, jar: jar, program: program,
		host: tap.host, port: tap.port,
		service: env.service, user: env.username, password: env.apiKey,
	}

	t.Run("byte quota tripped mid-result-set", func(t *testing.T) {
		// Every earlier connection's bytes are already persisted against the
		// grant they ran under; this one is fresh, so the budget below is spent
		// by the probe's own connect and fetches and nothing else.
		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaProbeBudget))

		run := probe
		run.mode = "quota"
		run.extra = []string{strconv.Itoa(quotaProbeFetch)}

		watchdogBefore := env.logs.count(logMsgWatchdogTeardown)

		output := run.run(t)

		frames := tappedOERs(t, tap.lastRecord(t).bytesFromServer())
		for _, f := range frames {
			t.Logf("tapped OER: ORA-%05d call=%d (call number readable: %t) %q",
				f.errorCode, f.callNumber, f.callNumberOK, f.message)
		}

		t.Logf("watchdog teardowns during the quota probe: %d",
			env.logs.count(logMsgWatchdogTeardown)-watchdogBefore)

		require.Contains(t, output, "read-ok 1",
			"the probe must get through its first statement before the quota bites:\n%s", output)
		require.NotContains(t, output, "QUOTA-NOT-TRIPPED",
			"the result set was drained without the quota ever tripping — the budget is too "+
				"generous for the payload and this measures nothing:\n%s", output)

		rowsBefore := probeRowsBeforeError(t, output)
		t.Logf("ojdbc drained %d of %d rows before the refusal", rowsBefore, quotaProbeRows)
		assert.Positive(t, rowsBefore,
			"the refusal must cut into a reply that was already streaming; zero rows means it "+
				"was refused at admission, which is the ordinary answering case:\n%s", output)
		assert.Less(t, rowsBefore, quotaProbeRows,
			"a full drain is not a mid-result-set refusal:\n%s", output)

		// What dbbat wrote. This half went exactly as the decision predicted:
		// the inline check in upstreamToClient fired (not the watchdog), it wrote
		// one ORA-00028, and it stamped the call number of the fetch the client
		// was parked in — read back off the wire, not off dbbat's own logs.
		assert.Zero(t, env.logs.count(logMsgWatchdogTeardown)-watchdogBefore,
			"a mid-reply trip must be caught by the inline check, which writes a frame, not by "+
				"the watchdog, which closes the socket")
		require.Len(t, frames, 1,
			"dbbat must have written exactly one OER into the reply it cut:\n%s", output)
		assert.Equal(t, 28, frames[0].errorCode, "the frame is the ORA-00028 session-terminated refusal")
		assert.True(t, frames[0].callNumberOK,
			"the frame must decode as a well-formed summary object")
		assert.NotZero(t, frames[0].callNumber,
			"the refusal must end the call the client is waiting on, not call zero")
		assert.Contains(t, frames[0].message, "bandwidth quota exceeded",
			"the frame must carry the real reason")

		// What ojdbc 26.1 did with it, which is the measurement and which does
		// **not** match the prediction. The driver never reports the ORA-00028:
		// it reports ORA-03113 "database connection closed by peer" with
		// last_rpc=Fetch a row, caused by ORA-17800 (read returned -1). So the
		// frame reaches the socket and is discarded unparsed.
		//
		// The reason is not the call number — that was the identified risk and
		// it is ruled out above and by the absence of ORA-18745 below, which is
		// the symptom a rejected call number produces. It is *where* the frame
		// is injected: dbbat cuts in at a TNS packet boundary, and a fetch reply
		// is a TTC message stream whose messages straddle packets, so the OER
		// lands inside a half-delivered row batch that the driver is still
		// consuming. go-ora reads it the same way (see the sibling subtest), so
		// this is a property of the injection point rather than of ojdbc.
		//
		// Pinned as measured rather than as wanted. The fix — hold the refusal
		// until the row stream reaches a message boundary — is
		// specs/todos/2026-08-13-01-mid-reply-refusal-lands-mid-ttc-message.md.
		assert.NotContains(t, output, "ORA-18745",
			"ORA-18745 is what a *rejected call number* looks like; the stamped number is "+
				"correct, so this must not appear:\n%s", output)
		assert.NotContains(t, output, "midfetch: code=28 ",
			"measured: ojdbc 26.1 does not surface the mid-reply ORA-00028. If this now fails, "+
				"the driver (or dbbat's injection point) changed and docs/oracle.md is stale:\n%s", output)
		assert.Contains(t, output, "midfetch: code=3113 ",
			"measured: the mid-reply frame is discarded and the session's close is what the "+
				"client reports:\n%s", output)
		assert.Contains(t, output, "done", "the probe must finish rather than throw:\n%s", output)
	})

	t.Run("go-ora reads the same mid-result-set trip", func(t *testing.T) {
		// The control for the case above: same refusal, same injection point, a
		// different driver. Whatever ojdbc does with a frame delivered inside a
		// half-written row batch, this says whether it is ojdbc's reading of it
		// or the frame's placement.
		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaProbeBudget))

		watchdogBefore := env.logs.count(logMsgWatchdogTeardown)

		rows, err := env.newClient(t).QueryContext(ctx, "SELECT id, payload FROM dbbat_quota_probe ORDER BY id")
		require.NoError(t, err, "the statement must be admitted; the quota is crossed while it streams")

		defer func() { _ = rows.Close() }()

		drained := 0
		for rows.Next() {
			drained++
		}

		err = rows.Err()

		t.Logf("go-ora drained %d of %d rows, then: %v (watchdog teardowns: %d)",
			drained, quotaProbeRows, err,
			env.logs.count(logMsgWatchdogTeardown)-watchdogBefore)

		require.Error(t, err, "go-ora drained the whole result set under a quota that should have cut it")
		assert.Positive(t, drained, "the refusal must have cut into a reply that was already streaming")
		assert.Less(t, drained, quotaProbeRows, "a full drain is not a mid-result-set refusal")
		assert.NotContains(t, err.Error(), "ORA-00028",
			"measured: go-ora does not surface the mid-reply frame either — it reports a dead "+
				"connection, the same as ojdbc. That is what makes the injection point, and not "+
				"either driver's error handling, the thing to fix")
	})

	t.Run("grant revoked while the client is idle", func(t *testing.T) {
		env.replaceGrant(t, nil)

		before := env.logs.count(logMsgWatchdogTeardown)

		run := probe
		run.mode = "idle"

		var idleQuiet, idleClosed bool

		run.onIdle = func(string) {
			// The client is parked between calls: no statement in flight and
			// nothing in its receive buffer. This is the only refusal path that
			// can fire there.
			record := tap.lastRecord(t)
			before := record.bytesSeen()

			env.revokeAllGrantsLive(t)
			time.Sleep(idleRevokeSettle)

			// The direct evidence the decision rests on: not one byte reached
			// the idle client. A "graceful" OER would show up here, and would
			// then be consumed as the answer to the *next* call — which is the
			// ORA-18745 mislabelling the close exists to avoid.
			idleQuiet = record.bytesSeen() == before
			idleClosed = record.closed()
		}

		output := run.run(t)

		require.Contains(t, output, "read-ok 1",
			"the probe must have been connected and working before the revocation:\n%s", output)
		assert.NotContains(t, output, "NO-INTERRUPTION",
			"a revoked grant must not answer another statement:\n%s", output)

		// dbbat wrote no frame: the watchdog force-closes both sockets, so what
		// the client meets is a dead socket and not an error message. An
		// ORA-00028 here would mean a frame was written on an idle socket, and
		// an ORA-18745 would be that frame being consumed as the answer to the
		// *next* call — the exact mislabelling the close avoids.
		assert.NotContains(t, output, "ORA-00028",
			"the idle path must write no OER; an ORA code here means one was sent:\n%s", output)
		assert.NotContains(t, output, "ORA-18745",
			"an out-of-sequence wrap is what writing a frame to an idle socket produces:\n%s", output)
		assert.True(t,
			strings.Contains(output, "ORA-17002") || strings.Contains(output, "ORA-03113"),
			"the client must see the close as a plain I/O error:\n%s", output)
		assert.Contains(t, output, "done", "the probe must finish rather than throw:\n%s", output)

		assert.True(t, idleQuiet,
			"dbbat wrote bytes to a client that was idle between calls; the watchdog path must "+
				"write no frame at all")
		assert.True(t, idleClosed,
			"the session must have been torn down by dropping the socket, which is what a real "+
				"Oracle's DISCONNECT SESSION does")

		assert.Greater(t, env.logs.count(logMsgWatchdogTeardown), before,
			"the teardown must have come from the limit watchdog (onLimitViolation), which is "+
				"the path that writes no frame")
	})
}

// requireOJDBC resolves the JDK and the ojdbc jar, skipping when either is
// missing — the same rule TestIntegration_BlockedStatementRefusesJDBCThin
// applies, and for the same reason: there is no packaged Oracle JDBC driver to
// look up, so CI has none.
func requireOJDBC(t *testing.T) (string, string) {
	t.Helper()

	java, err := exec.LookPath("java")
	if err != nil {
		t.Skipf("java unavailable: %v", err)
	}

	jar := oracleTestOJDBCJar(t)
	if jar == "" {
		t.Skipf("no Oracle JDBC driver: set %s to an ojdbc jar, or put one on CLASSPATH", ojdbcJarEnv)
	}

	return java, jar
}

// jdbcProbeRun is one run of AsyncProbe: where to dial, which mode, and what to
// do while it sits idle. A struct rather than eight positional parameters,
// because the two things a reader has to keep straight — the mode and the idle
// hook — would otherwise be lost among the coordinates.
type jdbcProbeRun struct {
	java    string
	jar     string
	program string

	// host/port are what the probe dials. Every case here goes through the
	// recording tap rather than straight at the proxy, so the frames dbbat
	// wrote are evidence rather than inference.
	host string
	port int

	// service/user/password are the connect coordinates. They live here rather
	// than being read off a fixture, because the real-Oracle capture points the
	// same program at a database with no dbbat in front of it.
	service  string
	user     string
	password string

	mode  string
	extra []string

	// onIdle, when non-nil, is what the measurement is about: the probe prints
	// `idle-ready` and blocks on stdin, so onIdle runs with the client provably
	// parked between calls — not merely "probably idle because the test slept".
	// It is handed everything the probe has printed so far, which is how the
	// real-Oracle capture learns the session id it has to kill. The probe is
	// released by a newline on stdin afterwards.
	onIdle func(printedSoFar string)
}

// run starts the probe and returns everything it printed.
func (r jdbcProbeRun) run(t *testing.T) string {
	t.Helper()

	args := append([]string{
		"-cp", r.jar, r.program,
		r.host, strconv.Itoa(r.port), r.service, r.user, r.password, r.mode,
	}, r.extra...)

	return runProbeProcess(t, r.java, args, r.onIdle)
}

// runProbeProcess is the process half of runJDBCProbe, shared with the
// real-Oracle capture, which points the same program at a raw tap instead of at
// the proxy.
func runProbeProcess(t *testing.T, java string, args []string, onIdle func(string)) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), jdbcRefusalDeadline)
	defer cancel()

	watcher := newLineWatcher("idle-ready")

	cmd := exec.CommandContext(ctx, java, args...)
	cmd.Stdout = watcher
	cmd.Stderr = watcher

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	require.NoError(t, cmd.Start())

	if onIdle != nil {
		select {
		case <-watcher.hit:
			onIdle(watcher.String())
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			t.Fatalf("the probe never reached its idle point:\n%s", watcher.String())
		}
	}

	_, _ = stdin.Write([]byte("go\n"))
	_ = stdin.Close()

	err = cmd.Wait()
	output := watcher.String()

	require.NoErrorf(t, err, "the JDBC probe did not finish cleanly:\n%s", output)

	return output
}

// lineWatcher collects a probe's output and fires a channel the first time a
// marker appears in it. exec's Stdout is an io.Writer, so this is the whole
// mechanism — no goroutine of its own, no scanner racing the process's exit.
type lineWatcher struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	fired  bool
	hit    chan struct{}
}

func newLineWatcher(marker string) *lineWatcher {
	return &lineWatcher{marker: marker, hit: make(chan struct{})}
}

func (w *lineWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, _ := w.buf.Write(p)

	if !w.fired && strings.Contains(w.buf.String(), w.marker) {
		w.fired = true

		close(w.hit)
	}

	return n, nil
}

func (w *lineWatcher) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
}

// probeRowsBeforeError reads back the `rows-before-error N` line the quota probe
// prints, which is what distinguishes a refusal cut into a streaming reply from
// one delivered at the statement gate.
func probeRowsBeforeError(t *testing.T, output string) int {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "rows-before-error ")
		if !ok {
			continue
		}

		n, err := strconv.Atoi(strings.TrimSpace(rest))
		require.NoError(t, err, "unreadable row count %q", line)

		return n
	}

	t.Fatalf("the probe never reported how far it got:\n%s", output)

	return 0
}
