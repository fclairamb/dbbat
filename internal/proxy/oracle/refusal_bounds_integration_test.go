//go:build integration

package oracle

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
)

// This file is where the two held-refusal fail-safe bounds get their numbers.
//
// refusalHoldMaxBytes (8 MiB) and refusalHandoffGrace (30s) were reasoned from
// "the tail of one fetch batch, milliseconds on a loopback" rather than
// observed, and the suites that drive live clients all measure the same corner
// of the space: a modest fetch size on a loopback, where both quantities are
// microscopic. Two things move them, and nothing else does:
//
//   - the **fetch size**, which sets how many bytes of reply are still in flight
//     when the quota is crossed — that is the whole of the byte cost;
//   - the **link speed**, which sets how long those bytes take to arrive — that
//     is the whole of the time cost, and loopback divides it to zero.
//
// So the two subtests below take one client (python-oracledb thin — no JVM, no
// OCI runtime, dials loopback) to each extreme in turn, and every other suite
// reports its own handoff cost through reportHandoffCost so the ordinary cases
// are on the record too. The results are in docs/oracle.md, "What a legitimate
// handoff costs, measured".

// The wide fixture the byte side is measured on. VARCHAR2(4000) is the largest
// non-LOB character column Oracle has, so wideProbeFetch rows of it is about as
// much reply as a single fetch round trip can be asked to carry — which is
// exactly the worst case refusalHoldMaxBytes has to survive.
const (
	wideProbeRows  = 3000
	wideProbeWidth = 4000
	wideProbeFetch = 3000
	wideProbeTable = "dbbat_wide_probe"
)

// throttledLinkBytesPerSecond is the link the time side is measured over: 1
// Mbit/s, a deliberately poor but entirely ordinary WAN. Paired with
// throttledProbeFetch below it puts a fetch batch of a few hundred kilobytes on
// a link slow enough that the handoff takes seconds rather than microseconds.
const (
	throttledLinkBytesPerSecond = 128 * 1024
	throttledProbeFetch         = 2000
)

// The link the *crossover* between the two bounds is measured over, which is a
// different question from the one above: not "what does a legitimate handoff
// cost on a poor link" but "what happens on a link so poor that the grace runs
// out before the byte bound ever could".
//
// 8 KiB/s is far below the ~280 KiB/s the two bounds meet at
// (refusalHoldMaxBytes / refusalHandoffGrace), and it is chosen so the tail left
// in flight is minutes of relaying rather than seconds — the abandonment has to
// be the *clock* and not a batch that happened to finish. crossoverProbeBudget
// is smaller than quotaClientBudget for the same reason the rate is low: the
// quota must trip early enough that the run is the 30s grace plus a little,
// while still outweighing python-oracledb's connect several times over so the
// statement is admitted and cut afterwards rather than refused at the gate.
const (
	crossoverLinkBytesPerSecond = 8 * 1024
	crossoverProbeBudget        = 150_000
	crossoverProbeDeadline      = 4 * refusalDeadline
)

// boundsProbeScript drains a table at a caller-chosen array size and reports how
// far it got. It is the quota probe from async_refusal_clients_integration_test.go
// with the table and the fetch size lifted out, because the whole point here is
// to vary exactly those two.
//
// prefetchrows is set alongside arraysize deliberately: python-oracledb's
// prefetch defaults to 2 rows regardless of arraysize, and a batch the driver
// never asked for cannot be the batch in flight when the quota trips.
const boundsProbeScript = `
import sys
import time
import oracledb

host, port, service, user, password, table, arraysize = sys.argv[1:8]
conn = oracledb.connect(user=user, password=password,
                        dsn=oracledb.makedsn(host, int(port), service_name=service))
cur = conn.cursor()

cur.execute("SELECT 1 FROM dual")
print("read-ok", cur.fetchall()[0][0], flush=True)

rows = 0
started = time.monotonic()
try:
    cur.arraysize = int(arraysize)
    cur.prefetchrows = int(arraysize)
    cur.execute("SELECT id, payload FROM %s ORDER BY id" % table)
    for _ in cur:
        rows += 1
    print("QUOTA-NOT-TRIPPED rows=%d" % rows, flush=True)
except Exception as e:
    print("rows-before-error", rows, flush=True)
    err = e.args[0] if e.args else None
    print("midfetch: code=%s full_code=%s class=%s msg=%s"
          % (getattr(err, "code", None), getattr(err, "full_code", None),
             type(e).__name__, str(e).strip().splitlines()[0]), flush=True)

print("drain-seconds %.3f" % (time.monotonic() - started), flush=True)

try:
    conn.close()
except Exception:
    pass

print("done", flush=True)
`

// TestIntegration_HeldRefusalHandoffCost is the measurement behind the two
// fail-safe bounds' values.
//
// One fixture, two subtests, each pushing one of the two variables that move a
// handoff's cost to a realistic extreme. Neither asserts a bound is respected —
// that is what the unit tests do — they report what the handoff *cost*, which is
// the input the bounds are chosen from.
func TestIntegration_HeldRefusalHandoffCost(t *testing.T) {
	env := startOracleThroughProxy(t, nil)
	ctx := context.Background()

	seedQuotaProbeTable(t, env)
	seedWideProbeTable(t, env)

	t.Run("the byte cost is one fetch batch, and a fetch batch can be megabytes", func(t *testing.T) {
		script := requirePythonOracleDB(t, "bounds.py", boundsProbeScript)

		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaClientBudget))

		tap := startRecordingTap(t, env.host, env.port)
		before := newRefusalCounters(env)

		runCtx, cancel := context.WithTimeout(ctx, wideProbeDeadline)
		defer cancel()

		output := runBoundsProbe(t, runCtx, script, tap, env, wideProbeTable, wideProbeFetch)

		logTappedOERs(t, tap)

		delta := before.since(t, env)
		cost := reportHandoffCost(t, before, "python-oracledb thin, arraysize/prefetch "+
			strconv.Itoa(wideProbeFetch)+" on "+strconv.Itoa(wideProbeWidth)+"-byte rows, loopback")

		require.Contains(t, output, "read-ok 1",
			"the client must get through its first statement before the quota bites:\n%s", outputTail(output))
		require.NotContains(t, output, "QUOTA-NOT-TRIPPED",
			"the result set was drained without the quota tripping; this measures nothing:\n%s",
			outputTail(output))

		assert.Positive(t, delta.held, "the violation must have been held for the client's next call")

		// The finding this subtest exists for. On the suites' ordinary fetch
		// size the overshoot is a couple of hundred kilobytes, which is what
		// made 8 MiB look like three orders of magnitude of slack. It is not:
		// the cost is the reply still in flight, the fetch size sets that, and a
		// client asking for thousands of wide rows puts megabytes there.
		assert.Greater(t, cost.bytes, int64(1<<20),
			"a large fetch size must drive the handoff's byte cost into the megabytes; if it does "+
				"not, the upstream is splitting the batch and this measures the split, not the "+
				"fetch size")

		// Which of the two outcomes it produced is *reported*, not asserted:
		// both are legitimate, and which one a large fetch size lands on is
		// precisely the question the bound's value answers.
		switch {
		case delta.abandoned > 0:
			t.Logf("the overshoot bound fired: a legitimate %d-row fetch batch outran "+
				"refusalHoldMaxBytes (%d), so the client met the socket close instead of the "+
				"ORA-00028", wideProbeFetch, int64(refusalHoldMaxBytes))
			assert.Greater(t, cost.bytes, int64(refusalHoldMaxBytes),
				"the abandonment must be the bound being crossed and nothing else")
		default:
			t.Logf("the handoff landed inside the overshoot bound: %d bytes of %d",
				cost.bytes, int64(refusalHoldMaxBytes))
			assert.Positive(t, delta.delivered+delta.unnameable,
				"exactly one of the delivery outcomes must have happened")
		}
	})

	t.Run("the time cost is that batch divided by the link speed", func(t *testing.T) {
		script := requirePythonOracleDB(t, "bounds.py", boundsProbeScript)

		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(quotaClientBudget))

		// The same client, the same fixture, over a 1 Mbit/s link. Nothing else
		// changes: the byte cost is whatever the fetch size makes it, and the
		// time cost is that divided by the rate.
		tap := startThrottledRecordingTap(t, env.host, env.port, throttledLinkBytesPerSecond)
		before := newRefusalCounters(env)
		crossoversBefore := env.logs.count(logMsgRefusalGraceOutranBytes)

		runCtx, cancel := context.WithTimeout(ctx, wideProbeDeadline)
		defer cancel()

		output := runBoundsProbe(t, runCtx, script, tap, env, "dbbat_quota_probe", throttledProbeFetch)

		logTappedOERs(t, tap)

		delta := before.since(t, env)
		cost := reportHandoffCost(t, before, fmt.Sprintf(
			"python-oracledb thin, arraysize/prefetch %d on %d-byte rows, %d KiB/s link",
			throttledProbeFetch, quotaProbeWidth, throttledLinkBytesPerSecond/1024))

		require.Contains(t, output, "read-ok 1",
			"the client must get through its first statement before the quota bites:\n%s", outputTail(output))
		require.NotContains(t, output, "QUOTA-NOT-TRIPPED",
			"the result set was drained without the quota tripping:\n%s", outputTail(output))

		assertQuotaWasHeldMidStream(t, delta)

		// The point of the throttle: on loopback this number is single-digit
		// milliseconds and says nothing. Anything measured in seconds here is
		// the quantity refusalHandoffGrace actually has to cover.
		assert.Greater(t, cost.millis, int64(500),
			"the throttle must have stretched the handoff into a range loopback cannot reach; "+
				"below this the link is not slow enough to be measuring anything")
		assert.Less(t, cost.millis, refusalHandoffGrace.Milliseconds(),
			"and a legitimate handoff on a poor link must still fit inside the grace — if it does "+
				"not, the grace is too small rather than generous")

		// The relationship the two bounds are read through, logged so the
		// arithmetic in docs/oracle.md is checkable against a real run rather
		// than asserted from theory.
		if cost.millis > 0 {
			t.Logf("effective handoff rate: %d bytes in %d ms = %.0f KiB/s (link set to %d KiB/s)",
				cost.bytes, cost.millis,
				float64(cost.bytes)/float64(cost.millis)*1000/1024,
				throttledLinkBytesPerSecond/1024)
		}

		// A poor link is not the crossover. This handoff landed inside the grace,
		// so nothing was cut by the clock and the record that says otherwise must
		// stay silent — see the subtest below for the case it is for.
		assert.Equal(t, crossoversBefore, env.logs.count(logMsgRefusalGraceOutranBytes),
			"a handoff that completed inside the grace must not be reported as a bound crossover")
	})

	t.Run("below the crossover rate the grace fires first, and says so", func(t *testing.T) {
		script := requirePythonOracleDB(t, "bounds.py", boundsProbeScript)

		env.replaceGrant(t, nil, testsupport.WithMaxBytesTransferred(crossoverProbeBudget))

		// The same probe again, over a link 16× slower. At 8 KiB/s the tail left
		// in flight when the quota trips is minutes of relaying, so the client is
		// still draining when refusalHandoffGrace runs out — the one case where
		// the two fail-safes constrain each other rather than each covering its
		// own failure, and the one refusalHoldMaxBytes cannot reach.
		tap := startThrottledRecordingTap(t, env.host, env.port, crossoverLinkBytesPerSecond)
		before := newRefusalCounters(env)
		crossoversBefore := env.logs.count(logMsgRefusalGraceOutranBytes)

		runCtx, cancel := context.WithTimeout(ctx, crossoverProbeDeadline)
		defer cancel()

		output := runBoundsProbe(t, runCtx, script, tap, env, "dbbat_quota_probe", throttledProbeFetch)

		logTappedOERs(t, tap)

		delta := before.since(t, env)

		require.Contains(t, output, "read-ok 1",
			"the client must get through its first statement before the quota bites:\n%s", outputTail(output))
		require.NotContains(t, output, "QUOTA-NOT-TRIPPED",
			"the result set was drained without the quota tripping:\n%s", outputTail(output))

		require.Positive(t, delta.held,
			"the violation must have been held for the client's next call")
		require.Positive(t, delta.watchdog,
			"and the grace must be what ended it — if the client got its ORA-00028, the link is "+
				"not slow enough for the tail to outlast refusalHandoffGrace and this measures nothing")
		assert.Zero(t, delta.abandoned,
			"the byte bound must NOT be what fired: that it cannot fire at this rate is the finding")

		cost := reportHandoffCost(t, before, fmt.Sprintf(
			"python-oracledb thin, arraysize/prefetch %d on %d-byte rows, %d KiB/s link (below crossover)",
			throttledProbeFetch, quotaProbeWidth, crossoverLinkBytesPerSecond/1024))

		assert.Less(t, cost.bytes, int64(refusalHoldMaxBytes),
			"the hold was abandoned well under the byte bound, which is the crossover: at this rate "+
				"the clock always wins")

		require.Equal(t, crossoversBefore+1, env.logs.count(logMsgRefusalGraceOutranBytes),
			"a client still draining when its grace ran out must be reported as the crossover, not "+
				"left indistinguishable from one that stopped talking")

		effective := env.logs.intsFor(logMsgRefusalGraceOutranBytes, logAttrEffectiveBytesPerSec)
		crossover := env.logs.intsFor(logMsgRefusalGraceOutranBytes, logAttrCrossoverBytesPerSec)
		require.NotEmpty(t, effective)
		require.NotEmpty(t, crossover)

		t.Logf("BOUND CROSSOVER — effective %d B/s against a crossover rate of %d B/s "+
			"(%d-byte bound over a %s grace)",
			effective[len(effective)-1], crossover[len(crossover)-1],
			int64(refusalHoldMaxBytes), refusalHandoffGrace)

		assert.Less(t, effective[len(effective)-1], crossover[len(crossover)-1],
			"the record's whole claim: the link was below the rate the two bounds meet at")
	})
}

// wideProbeDeadline is refusalDeadline with room for the two runs here, which
// are slow on purpose: one moves megabytes, the other runs over a link
// deliberately made poor.
const wideProbeDeadline = 3 * refusalDeadline

// runBoundsProbe runs the python probe against one table at one array size and
// returns everything it printed.
func runBoundsProbe(
	t *testing.T, ctx context.Context, script string,
	tap *recordingTap, env *oracleThroughProxy, table string, arraysize int,
) string {
	t.Helper()

	cmd := exec.CommandContext(ctx, "python3", script,
		tap.host, strconv.Itoa(tap.port), env.service, env.username, env.apiKey,
		table, strconv.Itoa(arraysize))

	out, runErr := cmd.CombinedOutput()
	output := string(out)

	t.Logf("python-oracledb exit: %v", runErr)
	t.Logf("python output (tail):\n%s", outputTail(output))

	require.NoError(t, ctx.Err(), "the probe never came back:\n%s", outputTail(output))
	require.NoErrorf(t, runErr, "the probe did not finish cleanly:\n%s", outputTail(output))

	return output
}

// seedWideProbeTable fills the widest table Oracle will let a single fetch batch
// be built from, under the permissive grant the fixture starts with.
func seedWideProbeTable(t *testing.T, env *oracleThroughProxy) {
	t.Helper()

	ctx := context.Background()

	_, _ = env.db.ExecContext(ctx, "DROP TABLE "+wideProbeTable)

	_, err := env.db.ExecContext(ctx, fmt.Sprintf(
		"CREATE TABLE %s (id NUMBER, payload VARCHAR2(%d))", wideProbeTable, wideProbeWidth))
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	// DBMS_RANDOM for the reason the narrow fixture gives: Oracle drops a column
	// unchanged from the previous row off the wire entirely, and a table that
	// streams as a few kilobytes cannot overflow anything.
	_, err = env.db.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO %s SELECT level, DBMS_RANDOM.STRING('x', %d) FROM dual CONNECT BY level <= %d",
		wideProbeTable, wideProbeWidth, wideProbeRows))
	require.NoError(t, err, "seeding the wide result set")
}

// reportHandoffCost reads back what *this run's* held refusal cost — the bytes
// relayed past the violation and how long the hold lasted — off whichever record
// resolved it, and logs it.
//
// Every suite that drives a live client through a held refusal calls this, so
// the table in docs/oracle.md is one of observations rather than of estimates.
// The reads are bounded below by the counter snapshot taken before the run,
// because the fixture is shared and every earlier subtest logged into the same
// handler.
//
// It fails rather than skips when nothing was recorded: a measurement that
// silently reports nothing is how a bound goes unjustified for a release.
func reportHandoffCost(t *testing.T, before refusalCounters, client string) handoffCost {
	t.Helper()

	for _, msg := range refusalResolutionMessages {
		seen := before.costsSeen[msg]
		bytesSeen := before.env.logs.intsFor(msg, logAttrRelayedBytesSince)
		millis := before.env.logs.intsFor(msg, logAttrHeldForMillis)

		if len(bytesSeen) <= seen || len(millis) <= seen {
			continue
		}

		cost := handoffCost{bytes: bytesSeen[len(bytesSeen)-1], millis: millis[len(millis)-1]}

		t.Logf("HANDOFF COST — %s: %d bytes past the violation (%.1f%% of the %d-byte bound), "+
			"%d ms held (%.2f%% of the %s grace)",
			client,
			cost.bytes, 100*float64(cost.bytes)/float64(refusalHoldMaxBytes), int64(refusalHoldMaxBytes),
			cost.millis, 100*float64(cost.millis)/float64(refusalHandoffGrace.Milliseconds()),
			refusalHandoffGrace)

		return cost
	}

	t.Fatalf("no held refusal recorded its cost: the %q/%q attributes are the whole measurement "+
		"the two bounds are sized against", logAttrRelayedBytesSince, logAttrHeldForMillis)

	return handoffCost{}
}
