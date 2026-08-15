//go:build integration

package oracle

import (
	"context"
	"database/sql"
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
//
// One run measures **one** OCI flavor, and that is structural rather than a
// choice here: plannedOCIClient is a sync.OnceValue decided before any container
// starts, so the flavor is a property of the process. The docs' claim that both
// were measured cost two runs — the default (an Instant Client on PATH wins when
// there is one) and `ORACLE_TEST_OCI_CLIENT=container` for the client bundled in
// the Oracle image. CI has no client on PATH, so CI measures the bundled one
// only; re-checking the Instant Client after a change to the fixed-width
// encoding means running this with `ORACLE_TEST_OCI_CLIENT=path` on a machine
// that has one.
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

		// What the handoff cost, which is what refusalHoldMaxBytes and
		// refusalHandoffGrace are sized against — see
		// refusal_bounds_integration_test.go and docs/oracle.md, "What a
		// legitimate handoff costs, measured".
		reportHandoffCost(t, before, fmt.Sprintf("%s, ARRAYSIZE 500 on %d-byte rows, loopback",
			oci.label, quotaProbeWidth))

		// The first of the two findings this subtest exists for, pinned rather
		// than logged. assertRefusalFrameShape below branches on the
		// delivered/unnameable split because either is a legitimate *shape*, so
		// on its own it would stay green if OCI ever flipped onto the fallback —
		// while docs/oracle.md went on claiming the delivered path. Measured on
		// both flavors, the OCI piggyback dbbat cannot name is what these
		// clients open a session with, never what they resume a drained fetch
		// with, so the call after a fetch reply is always nameable.
		assert.Zero(t, delta.unnameable,
			"an OCI session must reach the *delivered* path: the unnameable fallback closing the "+
				"socket instead is the outcome the measurement ruled out, and the one the docs "+
				"would then be wrong about")

		assertRefusalFrameShape(t, frames, delta)

		// The second, and the one no other assertion here can make. Everything
		// above amounts to "the session ended", which a closed socket produces
		// too. sqlplus is the only client that prints the ORA text unchanged
		// (python-oracledb relabels it DPY-4011, go-ora ErrBadConn), and it is
		// also the only one whose frame is in the fixed-width encoding — so this
		// line is the end-to-end evidence that encodeOERFixedWidth produces
		// something an OCI client *reads*, written from the client leg, which is
		// the moment this fix moved and the whole of the spec's first risk.
		assert.Contains(t, output, "ORA-00028: session terminated",
			"sqlplus must report the refusal as an ORA error rather than meet a dead socket; "+
				"without this the fixed-width frame could be unreadable and everything else here "+
				"would still pass:\n%s", outputTail(output))
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

		reportHandoffCost(t, before, fmt.Sprintf(
			"python-oracledb thin 3.4, arraysize %d on %d-byte rows, loopback",
			quotaProbeFetch, quotaProbeWidth))

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

// pythonAuthScript is the AUTH-phase probe: one connect that is going to be
// refused, reported off the driver's own exception *object* rather than its text.
//
// The object is the whole point. The failure this measures rendered as
// `DPY-5002: internal error: read integer of length 39 when expecting integer of
// no more than length 4` — an InternalError about dbbat's own message length,
// with no ORA code anywhere in it — so a probe that only grepped the text could
// not tell "the driver parsed the refusal" from "the driver could not parse the
// frame at all". `class` and `full_code` can.
const pythonAuthScript = `
import sys
import oracledb

host, port, service, user, password = sys.argv[1:6]

try:
    conn = oracledb.connect(user=user, password=password,
                            dsn=oracledb.makedsn(host, int(port), service_name=service))
    print("CONNECTED-UNEXPECTEDLY", flush=True)
    conn.close()
except Exception as e:
    err = e.args[0] if e.args else None
    print("auth: class=%s code=%s full_code=%s msg=%s"
          % (type(e).__name__, getattr(err, "code", None),
             getattr(err, "full_code", None), str(e).strip().splitlines()[0]), flush=True)

print("done", flush=True)
`

// sqlplusAuthScript is the same for sqlplus. sqlplus prints the ORA error of a
// failed login and exits, so the script body only has to be something it would
// have run had the login worked.
const sqlplusAuthScript = `SET PAGESIZE 0
SET FEEDBACK OFF
SET HEADING OFF
SELECT 'connected=' || 1 FROM dual;
EXIT
`

// TestIntegration_AuthRefusalAcrossClients measures the refusal a client meets at
// AUTH Phase 2 — the one dbbat writes before the session exists at all — against
// the three clients whose parsers differ.
//
// Nothing covered it before this: every refusal test in the suite runs against an
// authenticated session, and the AUTH-phase frame was left on a three-part
// invention of dbbat's own (`0x08`, a compressed ORA code, a CLR) that no client
// parses as an error. python-oracledb thin turned all three refusals into
// DPY-5002 and reported the length of dbbat's message as an integer width, which
// read as a driver bug rather than as "you have no grant"; see
// specs/todos/2026-08-13-21-*.md.
//
// The fixture is deliberately the whole chain rather than a socket pair: what a
// unit test cannot check is that these clients *accept* the frame, and the two
// halves that decide it — the summary's trailing field count and the OCI dialect
// — are chosen from the live negotiation.
//
// One fixture, three clients, one revoked grant: an Oracle container start costs
// minutes, and every subtest here wants the same store state (a user with a valid
// API key and no grant on the target), so nothing has to be swapped between them.
func TestIntegration_AuthRefusalAcrossClients(t *testing.T) {
	env := startOracleThroughProxyForOCI(t, nil)
	ctx := context.Background()

	// Every subtest below opens its own connection, and a session resolves its
	// grant at authentication — so revoking once here is what makes all of them
	// refusals. The fixture's own connection was authenticated before this and is
	// unaffected, which is also why nothing here uses it.
	env.revokeAllGrants(t)

	t.Run("python-oracledb thin reads the ORA code out of the refusal", func(t *testing.T) {
		script := requirePythonOracleDB(t, "auth.py", pythonAuthScript)

		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		out, runErr := exec.CommandContext(runCtx, "python3", script,
			env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey).CombinedOutput()
		output := string(out)

		t.Logf("python-oracledb exit: %v\n%s", runErr, outputTail(output))

		require.NoError(t, runCtx.Err(), "python-oracledb never came back from the AUTH refusal")
		require.NotContains(t, output, "CONNECTED-UNEXPECTEDLY",
			"the user holds no grant; the login must be refused")

		assert.Contains(t, output, "full_code=ORA-01045",
			"the refusal must arrive as the ORA code dbbat chose:\n%s", outputTail(output))
		assert.Contains(t, output, "class=DatabaseError",
			"an ORA error the driver parsed is a DatabaseError; an InternalError means it could "+
				"not read the frame at all — which is the whole defect:\n%s", outputTail(output))
		assert.Contains(t, output, "no active grant",
			"the actionable text is the reason this frame exists: the user has to be told to "+
				"request access:\n%s", outputTail(output))
		assert.NotContains(t, output, "DPY-5002",
			"DPY-5002 is the defect verbatim: the driver read dbbat's message length as an "+
				"integer width because the frame was not an error structure:\n%s", outputTail(output))
		assert.Contains(t, output, "done", "the probe must finish rather than throw")
	})

	t.Run("go-ora reads the ORA code out of the refusal", func(t *testing.T) {
		client, err := sql.Open("oracle", env.dsn)
		require.NoError(t, err)

		t.Cleanup(func() { _ = client.Close() })

		pingCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		err = client.PingContext(pingCtx)
		require.Error(t, err, "the user holds no grant; the login must be refused")
		require.NoError(t, pingCtx.Err(), "go-ora never came back from the AUTH refusal")

		t.Logf("go-ora reported: %v", err)

		// The claim is the ORA code, not the wording, and that is a measurement
		// rather than caution: go-ora reports `ORA-1045 error occur at position:
		// 0` — its own rendering of the code, with dbbat's sentence lost. Its
		// summary parser reads the message CLR straight after the wide RetCode
		// pair, and go-ora negotiates customHash like a modern client, so it finds
		// dbbat's two trailing 20c fields there and reads the first as an empty
		// message. The two layouts are mutually exclusive; see
		// authRefusalOERShape for why the tail is chosen for the client that
		// *crashes* without it over the one that loses a sentence.
		assert.Contains(t, err.Error(), "1045",
			"go-ora must surface the refusal's ORA code rather than a dead socket")
		assert.NotContains(t, err.Error(), "ORA-03113",
			"ORA-03113 (end-of-file on communication channel) is what a client reports when the "+
				"refusal frame was unreadable and the socket simply closed")
		assert.NotContains(t, err.Error(), "ORA-12566",
			"ORA-12566 is what a legacy-framed reject reads as")
	})

	t.Run("sqlplus reads the ORA code out of the refusal", func(t *testing.T) {
		oci := requireOCIClient(t, env)
		ociAuthModeNote(t)

		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		output, runErr := oci.run(t, runCtx, sqlplusAuthScript)

		t.Logf("%s exit: %v\n%s", oci.label, runErr, outputTail(output))

		require.NoError(t, runCtx.Err(),
			"sqlplus never came back — an OCI client handed a frame in the wrong dialect waits "+
				"for the rest of a response that has already arrived:\n%s", outputTail(output))
		require.NotContains(t, output, "connected=1", "the user holds no grant; the login must be refused")

		assertNoOCIAuthMalformation(t, output)

		assert.Contains(t, output, "ORA-01045",
			"sqlplus prints the ORA text unchanged, so this is the end-to-end evidence that the "+
				"AUTH refusal is readable in the OCI encoding too:\n%s", outputTail(output))
	})

	// Last, because it is the one case that needs a grant: the refusal it
	// measures happens *after* the challenge, and a user with no grant is turned
	// away before their key is ever looked at (authenticateClient checks the
	// grant first, which is why every subtest above reports ORA-01045 whatever
	// password it sends).
	t.Run("python-oracledb thin reads a refused key the same way", func(t *testing.T) {
		script := requirePythonOracleDB(t, "auth_badkey.py", pythonAuthScript)

		env.replaceGrant(t, nil)

		runCtx, cancel := context.WithTimeout(ctx, refusalDeadline)
		defer cancel()

		out, runErr := exec.CommandContext(runCtx, "python3", script,
			env.host, strconv.Itoa(env.port), env.service, env.username, "dbb_not-a-real-key").CombinedOutput()
		output := string(out)

		t.Logf("python-oracledb exit: %v\n%s", runErr, outputTail(output))

		require.NoError(t, runCtx.Err(), "python-oracledb never came back from the AUTH refusal")
		require.NotContains(t, output, "CONNECTED-UNEXPECTEDLY", "a bogus key must be refused")

		// The other half of the summary-shape decision: the challenge has gone out
		// by now, so the tail follows the verifier it was built with rather than
		// the negotiation (authChallengeIsModern). It is also the 39-byte message
		// whose length DPY-5002 used to report as an integer width.
		assert.Contains(t, output, "full_code=ORA-01017",
			"a refused key is the generic credentials error, and it has to arrive as one:\n%s", outputTail(output))
		assert.Contains(t, output, "class=DatabaseError", outputTail(output))
		assert.Contains(t, output, "invalid username/password",
			"the 39-byte message whose length DPY-5002 reported:\n%s", outputTail(output))
		assert.NotContains(t, output, "DPY-5002", outputTail(output))
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

	// costsSeen is how many handoff-cost records each resolving message had
	// already logged, so reportHandoffCost can read *this* run's numbers out of
	// a fixture every earlier subtest also logged into. Without it a subtest
	// that abandoned would be reported with the previous one's delivered cost.
	costsSeen map[string]int
}

// refusalResolutionMessages are the records that end a held refusal, in the
// order reportHandoffCost prefers them. Each carries the two cost attributes.
var refusalResolutionMessages = []string{
	logMsgRefusalDelivered, logMsgRefusalUnnameable,
	logMsgRefusalHandoffAbandoned, logMsgWatchdogTeardown,
}

func newRefusalCounters(env *oracleThroughProxy) refusalCounters {
	costsSeen := make(map[string]int, len(refusalResolutionMessages))
	for _, msg := range refusalResolutionMessages {
		costsSeen[msg] = len(env.logs.intsFor(msg, logAttrRelayedBytesSince))
	}

	return refusalCounters{
		env:        env,
		watchdog:   env.logs.count(logMsgWatchdogTeardown),
		held:       env.logs.count(logMsgRefusalHeld),
		delivered:  env.logs.count(logMsgRefusalDelivered),
		unnameable: env.logs.count(logMsgRefusalUnnameable),
		abandoned:  env.logs.count(logMsgRefusalHandoffAbandoned),
		costsSeen:  costsSeen,
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
