//go:build integration

package oracle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
)

// This file is the other two clients' half of the measurement in
// async_refusal_integration_test.go: what sqlplus (OCI) and python-oracledb thin
// do with a byte quota crossed *mid-result-set*, now that dbbat holds the
// refusal and answers the client's next call rather than writing it into the
// reply in progress.
//
// They are not a formality. Both have already been the source of a refusal dbbat
// framed correctly by every other test and the client still could not read —
// that is why oerShape carries fixedWidth, fixedWidth64 and endOfResponse at all
// — and this path moved the *write* to the client leg, which is a different
// moment even though the frame is byte-identical. Two risks were named going in:
//
//  1. sqlplus is the client that hangs rather than errors when a frame arrives
//     at a moment it does not expect;
//  2. answerHeldRefusal falls back to a bare socket close when the client's next
//     call is one dbbat cannot *name*, and the bundled OCI client's own
//     piggyback frames are exactly that shape (gateUnnameableFrame's reason for
//     existing). An OCI session taking that fallback and never seeing the
//     ORA-00028 was a live possibility rather than a theoretical one.
//
// Both are settled below by measurement, and the results are in docs/oracle.md.

// quotaClientBudget is the byte budget these two clients run under. It is
// smaller than the JDBC probe's, and for one reason only: sqlplus prints every
// row it fetches, so the budget is also roughly how much text the test has to
// carry. It still has to outweigh the client's connect several times over, or
// the statement would be refused at admission and this would measure the
// ordinary answering case.
const quotaClientBudget = 300_000

// sqlplusQuotaScript drains the seeded result set through sqlplus with an
// explicit array size, so the quota is crossed inside a *fetch* reply rather
// than at a statement boundary.
//
// The trailing SELECT is the finding, not decoration: it says whether the
// session outlived the refusal. A held refusal ends the session on every path,
// so it must not print — but a client that *hangs* prints nothing either, which
// is why the run's own exit is asserted separately.
const sqlplusQuotaScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SET HEADING OFF
SET LINESIZE 32767
SET LONG 32767
SET ARRAYSIZE 500
SELECT 'read-ok=' || 1 FROM dual;
SELECT payload FROM dbbat_quota_probe ORDER BY id;
SELECT 'after=' || 42 FROM dual;
EXIT
`

// pythonQuotaScript is the same case for python-oracledb thin. It reports the
// row count reached before the error, which is what distinguishes a refusal cut
// into a streaming reply from one delivered at the statement gate, and the ORA
// code off the driver's own error object rather than out of its message text.
const pythonQuotaScript = `
import sys
import oracledb

host, port, service, user, password, arraysize = sys.argv[1:7]
conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))
cur = conn.cursor()

cur.execute("SELECT 1 FROM dual")
print("read-ok", cur.fetchall()[0][0], flush=True)

rows = 0
try:
    cur.arraysize = int(arraysize)
    cur.execute("SELECT id, payload FROM dbbat_quota_probe ORDER BY id")
    for _ in cur:
        rows += 1
    print("QUOTA-NOT-TRIPPED rows=%d" % rows, flush=True)
except Exception as e:
    print("rows-before-error", rows, flush=True)
    err = e.args[0] if e.args else None
    print("midfetch: code=%s full_code=%s class=%s msg=%s"
          % (getattr(err, "code", None), getattr(err, "full_code", None),
             type(e).__name__, str(e).strip().splitlines()[0]), flush=True)

try:
    cur.execute("SELECT 42 FROM dual")
    print("after", cur.fetchall()[0][0], flush=True)
except Exception as e:
    print("after-failed:", str(e).strip().splitlines()[0], flush=True)

try:
    conn.close()
except Exception:
    pass

print("done", flush=True)
`

// TestIntegration_AsyncRefusalAgainstOCIAndPythonThin measures the held
// mid-reply refusal on the two clients TestIntegration_AsyncRefusalAgainstJDBCThin
// does not cover.
//
// One fixture, two subtests, for the same reason the JDBC measurement gives: an
// Oracle container start costs minutes and the fixture can swap the grant
// between them, since each subtest opens its own connection and a session
// resolves its grant once at auth.
//
// The fixture is the OCI one (startOracleThroughProxyForOCI) because sqlplus may
// have to run *inside* the Oracle container — that needs the proxy bound on
// every interface and a route back to the host, both creation-time decisions.
// python-oracledb dials loopback either way.
func TestIntegration_AsyncRefusalAgainstOCIAndPythonThin(t *testing.T) {
	env := startOracleThroughProxyForOCI(t, nil)
	ctx := context.Background()

	seedQuotaProbeTable(t, env)

	t.Run("sqlplus meets a byte quota mid-result-set", func(t *testing.T) {
		oci := requireOCIClient(t, env)
		ociAuthModeNote(t)

		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaClientBudget))

		// Through a tap this flavor can actually dial: "did dbbat write a
		// readable frame" and "did the client report it" are separate questions,
		// and only the first has an answer that does not go through the client.
		tap, host, port := oci.tap(t, env)

		before := newRefusalCounters(env)

		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		output, runErr := oci.runAt(t, runCtx, sqlplusQuotaScript, host, port)

		frames := logTappedOERs(t, tap)
		delta := before.since(t, env)

		t.Logf("%s exit: %v", oci.label, runErr)
		t.Logf("sqlplus output (tail):\n%s", outputTail(output))

		require.NoError(t, runCtx.Err(),
			"sqlplus never came back — a held refusal that hangs the client is worse than the "+
				"ORA-03113 it replaced:\n%s", outputTail(output))
		require.Contains(t, output, "read-ok=1",
			"the client must get through its first statement before the quota bites:\n%s", outputTail(output))
		rowsPrinted := countPayloadLines(output)
		t.Logf("sqlplus printed %d of %d rows before the refusal", rowsPrinted, quotaProbeRows)
		assert.Positive(t, rowsPrinted,
			"the refusal must cut into a reply that was already streaming; zero rows means it was "+
				"refused at admission, which is the ordinary answering case")
		assert.Less(t, rowsPrinted, quotaProbeRows, "a full drain is not a mid-result-set refusal")

		assertQuotaWasHeldMidStream(t, delta)
		assertRefusalFrameShape(t, frames, delta)

		assert.NotContains(t, output, "after=42",
			"a held refusal ends the session; the connection must not answer another statement")
	})

	t.Run("python-oracledb thin meets a byte quota mid-result-set", func(t *testing.T) {
		script := requirePythonOracleDB(t, "quota.py", pythonQuotaScript)

		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaClientBudget))

		tap := startRecordingTap(t, env.host, env.port)

		before := newRefusalCounters(env)

		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		cmd := exec.CommandContext(runCtx, "python3", script,
			tap.host, strconv.Itoa(tap.port), env.service, env.username, env.apiKey,
			strconv.Itoa(quotaProbeFetch))

		out, runErr := cmd.CombinedOutput()
		output := string(out)

		frames := logTappedOERs(t, tap)
		delta := before.since(t, env)

		t.Logf("python-oracledb exit: %v", runErr)
		t.Logf("python output (tail):\n%s", outputTail(output))

		require.NoError(t, runCtx.Err(),
			"python-oracledb never came back from the mid-result-set refusal:\n%s", outputTail(output))
		require.NoErrorf(t, runErr,
			"the python probe did not finish cleanly:\n%s", outputTail(output))
		require.Contains(t, output, "read-ok 1",
			"the client must get through its first statement before the quota bites:\n%s", outputTail(output))
		require.NotContains(t, output, "QUOTA-NOT-TRIPPED",
			"the result set was drained without the quota tripping; this measures nothing")

		rowsBefore := probeRowsBeforeError(t, output)
		t.Logf("python-oracledb drained %d of %d rows before the refusal", rowsBefore, quotaProbeRows)
		assert.Positive(t, rowsBefore,
			"the refusal must cut into a reply that was already streaming; zero rows means it was "+
				"refused at admission, which is the ordinary answering case")
		assert.Less(t, rowsBefore, quotaProbeRows, "a full drain is not a mid-result-set refusal")

		assertQuotaWasHeldMidStream(t, delta)
		assertRefusalFrameShape(t, frames, delta)

		// The measurement itself, and it needed the error *object* rather than
		// the error text to make. python-oracledb parsed the ORA-00028 — its
		// `code` is 28 — and then rendered the exception as its own DPY-4011
		// "the database or network closed the connection", because it treats
		// ORA-00028 as a dead session much as go-ora does. So the message text
		// alone would have read as the pre-fix ORA-03113 failure, and `code`
		// is what separates "parsed the frame" from "met a closed socket".
		assert.Contains(t, output, "midfetch: code=28 ",
			"the whole point of holding the refusal for a call boundary: python-oracledb must "+
				"report the ORA-00028 rather than a dead socket:\n%s", outputTail(output))
		assert.Contains(t, output, "full_code=DPY-4011",
			"pinned as measured, not as preferred: the parsed ORA-00028 is surfaced under "+
				"python-oracledb's own connection-closed code. A change here means the driver "+
				"stopped folding a killed session into DPY-4011, and the doc rows are stale:\n%s",
			outputTail(output))
		assert.NotContains(t, output, "after 42",
			"a held refusal ends the session; the connection must not answer another statement")
		assert.Contains(t, output, "done", "the probe must finish rather than throw")
	})
}

// seedQuotaProbeTable creates and fills the table the byte quota is tripped in,
// under the permissive grant the fixture starts with — the seed would otherwise
// consume the budget it exists to overflow.
//
// DBMS_RANDOM rather than a constant is load-bearing; see the note on the JDBC
// probe's seeding, where RPAD('x', 400, 'x') streamed as a few kilobytes because
// Oracle compresses a column unchanged from the previous row out of the wire.
func seedQuotaProbeTable(t *testing.T, env *oracleThroughProxy) {
	t.Helper()

	ctx := context.Background()

	_, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_quota_probe")

	_, err := env.db.ExecContext(ctx,
		fmt.Sprintf("CREATE TABLE dbbat_quota_probe (id NUMBER, payload VARCHAR2(%d))", quotaProbeWidth))
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	_, err = env.db.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO dbbat_quota_probe SELECT level, DBMS_RANDOM.STRING('x', %d) FROM dual CONNECT BY level <= %d",
		quotaProbeWidth, quotaProbeRows))
	require.NoError(t, err, "seeding the result set the quota is tripped in")
}

// requirePythonOracleDB writes a python-oracledb script to a temp file and hands
// back its path, skipping loudly when the driver is not importable.
//
// Loudly matters: python-oracledb is not installed in CI, and a silent skip on
// the *only* measurement of a client is exactly how the OCI coverage gap in
// oci_client_integration_test.go survived as long as it did.
func requirePythonOracleDB(t *testing.T, name, source string) string {
	t.Helper()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v — this client goes unmeasured on this machine", err)
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skipf("python-oracledb not installed (pip install oracledb): %v — "+
			"this client goes unmeasured on this machine", err)
	}

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))

	return path
}

// refusalCounters samples the four log counters that say which path a held
// refusal took, so a subtest reads deltas rather than absolutes — the fixture is
// shared and every earlier subtest's sessions logged into the same handler.
type refusalCounters struct {
	env *oracleThroughProxy

	watchdog   int
	held       int
	delivered  int
	unnameable int
	abandoned  int
}

func newRefusalCounters(env *oracleThroughProxy) refusalCounters {
	return refusalCounters{
		env:        env,
		watchdog:   env.logs.count(logMsgWatchdogTeardown),
		held:       env.logs.count(logMsgRefusalHeld),
		delivered:  env.logs.count(logMsgRefusalDelivered),
		unnameable: env.logs.count(logMsgRefusalUnnameable),
		abandoned:  env.logs.count(logMsgRefusalHandoffAbandoned),
	}
}

// since returns what happened between the sample and now, and logs it — the
// counters are the measurement as much as the client's output is, and a run that
// took the unnameable fallback has to say so in the log whether or not any
// assertion happens to fail.
func (c refusalCounters) since(t *testing.T, env *oracleThroughProxy) refusalCounters {
	t.Helper()

	now := newRefusalCounters(env)
	delta := refusalCounters{
		watchdog:   now.watchdog - c.watchdog,
		held:       now.held - c.held,
		delivered:  now.delivered - c.delivered,
		unnameable: now.unnameable - c.unnameable,
		abandoned:  now.abandoned - c.abandoned,
	}

	t.Logf("refusal path: held=%d delivered=%d unnameable=%d over-bytes=%d watchdog=%d",
		delta.held, delta.delivered, delta.unnameable, delta.abandoned, delta.watchdog)

	return delta
}

// assertQuotaWasHeldMidStream pins the half that is dbbat's own behavior and is
// the same for every client: the inline check caught the crossing and held the
// violation for a call boundary, rather than the watchdog dropping the socket or
// the relay running out of its overshoot bound.
func assertQuotaWasHeldMidStream(t *testing.T, delta refusalCounters) {
	t.Helper()

	assert.Positive(t, delta.held,
		"the refusal must have been held rather than written into the reply in progress")
	assert.Zero(t, delta.watchdog,
		"a mid-reply trip must be caught by the inline check, which holds the refusal for the "+
			"client's next call, not by the watchdog, which closes the socket")
	assert.Zero(t, delta.abandoned,
		"the client spoke again well inside refusalHoldMaxBytes; the overshoot bound firing here "+
			"would mean the reply never reached a boundary at all")
	assert.Equal(t, 1, delta.delivered+delta.unnameable,
		"exactly one of the two delivery outcomes must have happened: the frame went out on a "+
			"call dbbat could name, or the session was closed on one it could not")
}

// assertRefusalFrameShape pins what went on the wire, which is the question a
// client's own error text cannot answer: a frame never written and a frame
// written and unread look identical from there.
//
// It is written around the delivered/unnameable split rather than assuming one,
// because the unnameable fallback is a legitimate outcome for an OCI client —
// see the note at the top of this file.
func assertRefusalFrameShape(t *testing.T, frames []tappedOER, delta refusalCounters) {
	t.Helper()

	var refusals []tappedOER

	for _, f := range frames {
		if f.errorCode == int(ORA00028) {
			refusals = append(refusals, f)
		}
	}

	if delta.unnameable > 0 {
		assert.Zero(t, delta.delivered,
			"a session takes one delivery path, not both")
		assert.Empty(t, refusals,
			"the unnameable fallback writes no frame: an OER stamped with a stale call number "+
				"ends a call the client is not parked on")

		return
	}

	assert.Positive(t, delta.delivered,
		"the held refusal must have been delivered on the client's next call")
	require.Len(t, refusals, 1,
		"dbbat must have written exactly one ORA-00028, at a call boundary")
	assert.True(t, refusals[0].callNumberOK,
		"the frame must decode as a well-formed summary object")
	assert.NotZero(t, refusals[0].callNumber,
		"the refusal must end the call the client is waiting on, not call zero")
	assert.Contains(t, refusals[0].message, "bandwidth quota exceeded",
		"the frame must carry the real reason")
}

// logTappedOERs decodes and logs every OER the tapped connection carried, and
// hands them back. Logging them unconditionally is deliberate: an unexpected
// extra frame is a finding even when the assertions that follow pass.
func logTappedOERs(t *testing.T, tap *recordingTap) []tappedOER {
	t.Helper()

	frames := tappedOERs(t, tap.lastRecord(t).bytesFromServer())
	for _, f := range frames {
		t.Logf("tapped OER: ORA-%05d call=%d (call number readable: %t) %q",
			f.errorCode, f.callNumber, f.callNumberOK, f.message)
	}

	return frames
}

// countPayloadLines counts the seeded rows a client printed. sqlplus has no
// equivalent of the probe's `rows-before-error` line — it prints rows and
// nothing else — so the payload's own width is what identifies one: every seeded
// value is exactly quotaProbeWidth random characters, and nothing else the
// script prints comes close.
func countPayloadLines(output string) int {
	rows := 0

	for _, line := range strings.Split(output, "\n") {
		if len(strings.TrimRight(line, "\r")) == quotaProbeWidth {
			rows++
		}
	}

	return rows
}

// outputTail keeps an assertion message readable when the client printed the
// result set it was draining — sqlplus prints every row it fetched, which under
// quotaClientBudget is a few hundred kilobytes of random payload.
func outputTail(output string) string {
	const keep = 3000

	if len(output) <= keep {
		return output
	}

	trimmed := output[len(output)-keep:]
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}

	return fmt.Sprintf("… (%d earlier bytes elided)\n%s", len(output)-len(trimmed), trimmed)
}
