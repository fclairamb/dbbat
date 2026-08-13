//go:build integration

package oracle

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	go_ora "github.com/sijms/go-ora/v3"
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
// ojdbc does not swallow an error whose call number it disagrees with — it
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
//  1. a byte quota crossed while dbbat is relaying a reply — a refusal with
//     nowhere readable to go, because the client is mid-way through consuming a
//     TTC message stream. dbbat holds it until the client announces a boundary
//     by sending its next call and answers *that* with an ORA-00028 stamped with
//     its number. The measurement settles two things at once: that the driver
//     surfaces the error rather than a dead socket, and that it accepts the
//     number rather than mislabelling the error as ORA-18745;
//  2. a grant revoked while the client sits idle between calls — the one path
//     where there is no call to end, where dbbat deliberately writes no frame
//     at all and force-closes both sockets instead.
//
// The driver has to be one that checks the OER's call number against the
// sequence number it sent, or both cases would pass without proving anything —
// ojdbc 23.2 does not check. Measured here on **ojdbc 23.7.0.25.01**, the jar
// SQLcl 26.1 bundles, which does: its `T4CTTIfun.receive` compares
// `T4CTTIoer11.callNumber` with its own `sequenceNumber` and routes a mismatch
// to `handleOutOfSequenceError` ("TTIOER call number {0} does not match TTIFUN
// sequence number {1}"), the path that produces ORA-18745. The 26.1 the
// original call-number finding was made on was not reachable on this machine;
// see docs/oracle.md for the caveat that leaves.
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
		heldBefore := env.logs.count(logMsgRefusalHeld)
		deliveredBefore := env.logs.count(logMsgRefusalDelivered)

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

		// What dbbat wrote. The violation is caught by the inline check (not the
		// watchdog), held while the reply in flight finishes, and written as one
		// ORA-00028 answering the client's *next* call — stamped with that
		// call's number, read back off the wire rather than off dbbat's logs.
		// Exactly one frame either way: holding does not turn into pushing.
		assert.Zero(t, env.logs.count(logMsgWatchdogTeardown)-watchdogBefore,
			"a mid-reply trip must be caught by the inline check, which holds the refusal for the "+
				"client's next call, not by the watchdog, which closes the socket")
		assert.Positive(t, env.logs.count(logMsgRefusalHeld)-heldBefore,
			"the refusal must have been held rather than written into the reply in progress")
		assert.Positive(t, env.logs.count(logMsgRefusalDelivered)-deliveredBefore,
			"the held refusal must have been delivered on the client's next call")
		require.Len(t, frames, 1,
			"dbbat must have written exactly one OER:\n%s", output)
		assert.Equal(t, 28, frames[0].errorCode, "the frame is the ORA-00028 session-terminated refusal")
		assert.True(t, frames[0].callNumberOK,
			"the frame must decode as a well-formed summary object")
		assert.NotZero(t, frames[0].callNumber,
			"the refusal must end the call the client is waiting on, not call zero")
		assert.Contains(t, frames[0].message, "bandwidth quota exceeded",
			"the frame must carry the real reason")

		// What ojdbc does with it, which is the measurement.
		//
		// Before the fix this read the other way round — ORA-03113
		// "database connection closed by peer" (last_rpc=Fetch a row, cause
		// ORA-17800), with the ORA-00028 written and discarded unparsed. The
		// call number was never the problem (ORA-18745 is what a rejected one
		// looks like, and it never appeared); *where* the frame went was. dbbat
		// cut in at a TNS packet boundary while a fetch reply is a TTC message
		// stream whose messages straddle packets, so the OER landed inside a
		// half-delivered row batch and was consumed as row bytes. go-ora read it
		// the same way, which is what made the injection point and not either
		// driver's parser the thing to fix.
		//
		// The refusal now waits for the client to announce a boundary by sending
		// its next call, and answers that — see session.enforceMidStreamLimits.
		assert.NotContains(t, output, "ORA-18745",
			"ORA-18745 is what a *rejected call number* looks like; the stamped number is "+
				"correct, so this must not appear:\n%s", output)
		assert.Contains(t, output, "midfetch: code=28 ",
			"the whole point of holding the refusal for a call boundary: ojdbc must now report "+
				"the ORA-00028 rather than a dead socket:\n%s", output)
		assert.NotContains(t, output, "midfetch: code=3113 ",
			"a plain I/O error means the frame was written somewhere the driver could not read "+
				"it, which is the defect this measures:\n%s", output)
		assert.Contains(t, output, "done", "the probe must finish rather than throw:\n%s", output)
	})

	t.Run("go-ora reads the same mid-result-set trip", func(t *testing.T) {
		// The control for the case above: same refusal, same delivery point, a
		// different driver. It is what established that the *old* frame's
		// placement — and not ojdbc's reading of it — was the defect.
		//
		// What it can no longer establish is the fix, and the reason is in the
		// driver rather than in dbbat: go-ora maps ORA-00028 to a dead
		// connection **by design**. `network.OracleError.Bad()` lists 28
		// alongside 3113/3114/12537, and `defaultStmt.fetch` turns any error
		// `isBadConn` accepts into `driver.ErrBadConn` — so a mid-result-set
		// ORA-00028 reaches the caller as "driver: bad connection" whether it
		// was parsed or never arrived. The error text cannot tell those two
		// apart, so this subtest stops asking it to: it runs go-ora through its
		// own recording tap and asserts on what dbbat wrote and where.
		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaProbeBudget))

		watchdogBefore := env.logs.count(logMsgWatchdogTeardown)
		heldBefore := env.logs.count(logMsgRefusalHeld)
		deliveredBefore := env.logs.count(logMsgRefusalDelivered)

		goTap := startRecordingTap(t, env.host, env.port)

		client, err := sql.Open("oracle",
			go_ora.BuildUrl(goTap.host, goTap.port, env.service, env.username, env.apiKey, nil))
		require.NoError(t, err)

		client.SetMaxOpenConns(1)
		client.SetMaxIdleConns(1)

		defer func() { _ = client.Close() }()

		require.NoError(t, client.PingContext(ctx))

		rows, err := client.QueryContext(ctx, "SELECT id, payload FROM dbbat_quota_probe ORDER BY id")
		require.NoError(t, err, "the statement must be admitted; the quota is crossed while it streams")

		defer func() { _ = rows.Close() }()

		drained := 0
		for rows.Next() {
			drained++
		}

		err = rows.Err()

		frames := tappedOERs(t, goTap.lastRecord(t).bytesFromServer())

		var refusals []tappedOER

		for _, f := range frames {
			t.Logf("tapped OER: ORA-%05d call=%d (call number readable: %t) %q",
				f.errorCode, f.callNumber, f.callNumberOK, f.message)

			if f.errorCode == int(ORA00028) {
				refusals = append(refusals, f)
			}
		}

		t.Logf("go-ora drained %d of %d rows, then: %v (watchdog teardowns: %d)",
			drained, quotaProbeRows, err,
			env.logs.count(logMsgWatchdogTeardown)-watchdogBefore)

		require.Error(t, err, "go-ora drained the whole result set under a quota that should have cut it")
		assert.Positive(t, drained, "the refusal must have cut into a reply that was already streaming")
		assert.Less(t, drained, quotaProbeRows, "a full drain is not a mid-result-set refusal")

		// dbbat's side, which is the half this subtest can still measure: the
		// violation was held rather than written into the reply, and the frame
		// went out on the client's next call.
		assert.Zero(t, env.logs.count(logMsgWatchdogTeardown)-watchdogBefore,
			"the inline check must have caught it, not the watchdog")
		assert.Positive(t, env.logs.count(logMsgRefusalHeld)-heldBefore,
			"the refusal must have been held rather than written into the reply in progress")
		assert.Positive(t, env.logs.count(logMsgRefusalDelivered)-deliveredBefore,
			"the held refusal must have been delivered on the client's next call")
		require.Len(t, refusals, 1,
			"dbbat must have written exactly one ORA-00028, at a call boundary")
		assert.True(t, refusals[0].callNumberOK, "the frame must decode as a well-formed summary object")
		assert.Contains(t, refusals[0].message, "bandwidth quota exceeded", "and carry the real reason")

		// The driver's side, pinned as what it is rather than as what would be
		// nicer: go-ora reports a dead connection for ORA-00028 on purpose. If
		// this ever stops holding, go-ora changed OracleError.Bad() and the
		// error text becomes usable evidence again.
		assert.ErrorIs(t, err, driver.ErrBadConn,
			"go-ora maps ORA-00028 to driver.ErrBadConn (network.OracleError.Bad), so the ORA "+
				"code cannot reach the caller through database/sql:\n%v", err)
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
