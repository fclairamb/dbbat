# Measure the held mid-reply refusal on the other two clients, and its two fail-safes at all

## Goal

Close the two coverage gaps the held mid-reply refusal shipped with
(`session.enforceMidStreamLimits` / `answerHeldRefusal`, added by
`specs/done/.../2026-08-13-01-mid-reply-refusal-lands-mid-ttc-message.md`):

1. measure what sqlplus (OCI) and python-oracledb thin do with a byte quota
   crossed mid-result-set, now that dbbat holds the refusal and answers the
   client's *next* call instead of writing it into the reply in progress;
2. exercise the two bounds on that hold — `refusalHoldMaxBytes` (8 MiB) and
   `refusalHandoffGrace` (30s) — against something other than hand-mutated
   session state.

## Why

The fix is measured on two of the four clients dbbat supports:

- **ojdbc 23.7** reports `ORA-00028` where it used to report ORA-03113 — the
  acceptance measurement;
- **go-ora v3** cannot report it and never could: it maps ORA-00028 to
  `driver.ErrBadConn` on purpose (`network.OracleError.Bad()` lists 28 next to
  3113/3114/12537), so its subtest asserts on the tapped frame instead.

sqlplus and python-oracledb are unmeasured on this path, and both have already
been the source of a refusal that dbbat framed *correctly by every other test*
and the client still could not read — that is the entire reason `oerShape`
carries `fixedWidth`, `fixedWidth64` and `endOfResponse`. Two specific risks:

1. the refusal is now written from the **client leg** rather than the response
   leg. Nothing about the frame changed, but the moment it is written did, and
   sqlplus is the client that hangs rather than errors when a frame arrives at a
   moment it does not expect;
2. `answerHeldRefusal` falls back to a bare socket close when the client's next
   call is one dbbat **cannot name** — an unwalkable piggyback. The bundled OCI
   client's own first message is exactly such a frame
   (`gateUnnameableFrame`'s reason for existing), so an OCI session may well take
   the fallback and never see the ORA-00028 at all. Whether it does is a
   measurement, not a guess.

### And the two bounds have never fired outside a unit test

`refusalHoldMaxBytes` (8 MiB relayed past the violation) and
`refusalHandoffGrace` (30s waiting for the client to speak) are the fail-safes
that keep "hold the refusal" from becoming an enforcement hole — they are what
bounds the worst case at ≤30s and ≤8 MiB instead of the ~250ms the watchdog used
to give. **Neither has ever fired end to end**, and neither is reachable from a
live client on demand: a real upstream always ends its reply, and a real client
always speaks again. So the two tests that cover them reach them by **mutating
internal state**:

- `TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed` subtracts
  `refusalHoldMaxBytes + 1` from the held refusal's own `atBytes` mark
  (`limits_test.go`) and then calls `enforceMidStreamLimits` directly;
- `TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking` backdates
  `held.armedAt` past the grace (`backdateHeldRefusal`) and then calls
  `onLimitViolation` directly.

Both pin the arithmetic and the teardown, and neither pins that the *relay* and
the *watchdog* actually reach those calls with the values they would carry in a
live session. Note also that both constants are scope this fix added on its own
judgement — the spec asked for a boundary, not for bounds on waiting for one — so
their values (8 MiB, 30s) are reasoned, not measured.

No GitHub issue yet — one should be filed.

## Implementation

- Add the quota-mid-result-set case to the existing per-client integration
  suites rather than a new fixture: `oci_client_integration_test.go` /
  `oci_instantclient_test.go` for sqlplus, and the python-oracledb path used by
  `blocked_integration_test.go`. The fixture work is already there
  (`startOracleThroughProxy`, `testsupport.WithMaxBytesTransferred`, the
  `dbbat_quota_probe` table seeding in `async_refusal_integration_test.go`).
- Put each client behind `startRecordingTap` (`tap_test.go`) so "dbbat wrote a
  readable frame" and "the client reported it" stay separable — that separation
  is what turned the go-ora result from a false negative into a finding.
- Assert the log counters as well as the client output: `logMsgRefusalHeld` then
  `logMsgRefusalDelivered` is the delivered path, `logMsgRefusalUnnameable` is
  the close-instead fallback. If OCI lands on the fallback, that is the result to
  record — and then the follow-up question is whether the held refusal should
  wait for a *nameable* call rather than give up on the first unnameable one.
- Record what is measured in `docs/oracle.md`, "An asynchronous refusal: which
  call number, and whether to send one at all", next to the ojdbc and go-ora
  rows — including the paragraph there that currently names both of these gaps.

For the two bounds, an end-to-end exercise needs an upstream that misbehaves on
purpose, which the unit harness can already stand up and the integration one
cannot:

- **`refusalHoldMaxBytes`**: drive `upstreamToClient` over a `net.Pipe` (as
  `TestUpstreamToClient_ByteLimitHoldsRatherThanCuttingIn` does) with a fake
  upstream that streams *more than 8 MiB* of Data packets after the cap is
  crossed, and assert the relay returns the violation, having relayed a bounded
  amount. ~8 MiB of pipe writes in 40-byte packets is ~200k iterations; size the
  packets up (a few KB each) to keep it a second or two, or make the constant a
  package-level `var` so the test can shrink it — the second option is cheaper
  and is what makes the *relay*, not the arithmetic, the thing under test.
- **`refusalHandoffGrace`**: same treatment. With the constant injectable, run
  the real `guard.Watch` → `onLimitViolation` path with a millisecond grace and
  a client that never speaks, and assert both sockets are dropped and the
  statement is recorded `aborted: …` — which is the path a stuck client really
  takes, rather than a direct call with a backdated timestamp.

Making the two constants injectable (session fields defaulted from the consts,
or package `var`s) is the single change that unlocks both; do that first.

## Implementation Plan

Written after reading the code; ordered so the enabling change lands first.

1. **Make the two bounds injectable** (`internal/proxy/oracle/session.go`).
   Two new session fields — `refusalHoldBytes int64`, `refusalHoldGrace
   time.Duration` — read through accessors that fall back to the existing
   `refusalHoldMaxBytes` / `refusalHandoffGrace` consts when the field is zero.
   Fallback-on-zero rather than a constructor default, because a session is
   built by hand in a dozen unit tests and a bare `&session{}` must keep the
   production bounds.

2. **`refusalHoldMaxBytes` end to end** — extend
   `TestUpstreamToClient_ByteLimitHoldsRatherThanCuttingIn`'s harness into a new
   `TestUpstreamToClient_StopsRelayingPastTheOvershootBound`: a fake upstream
   over `net.Pipe` that streams without end, the bound shrunk to a few hundred
   bytes, and the assertion that `upstreamToClient` *returns*
   `ErrByteQuotaExceeded` having relayed a bounded amount, with the statement
   finalized. The existing arithmetic test stays (it pins the subtraction), but
   the relay is now what reaches the bound.

3. **`refusalHandoffGrace` end to end** — a new
   `TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut` running the real
   `guard.Watch` → `onLimitViolation` with a millisecond grace and a client that
   never speaks: both sockets dropped, and — through a
   `recordingCompletionStore` — the statement written with `aborted: bandwidth
   quota exceeded for this grant`. No `backdateHeldRefusal`.

4. **sqlplus (OCI) measurement** — a new subtest in the OCI integration file
   driving the quota-mid-result-set case through `startRecordingTap`, seeding
   `dbbat_quota_probe` the way the JDBC probe does, and asserting the *log
   counters* (`logMsgRefusalHeld` → `logMsgRefusalDelivered` vs
   `logMsgRefusalUnnameable`) plus the tapped frames. The client's own output is
   recorded as a finding either way; the fallback is a legitimate result.

5. **python-oracledb thin measurement** — the same case as a python script next
   to `pythonRefusalScript`, behind the same "skip loudly when oracledb is not
   importable" rule, through its own tap, with the same counter assertions.

6. **`docs/oracle.md`** — replace the confidence paragraph's "both of these
   gaps" with the measured rows, and add sqlplus/python-oracledb to the
   per-client table.
