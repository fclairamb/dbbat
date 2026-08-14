# `TestIntegration_HeldRefusalHandoffCost`'s crossover subtest is red

## Goal

Work out whether `reportRefusalBoundCrossover` stopped firing or the subtest's
premise stopped holding on the machines it runs on, and make
`below the crossover rate the grace fires first, and says so` green — or, if the
finding is that the crossover is no longer reachable at these values, re-express
what the test measures.

## Why

`make test-e2e-oracle` fails on exactly one subtest:

```
--- FAIL: TestIntegration_HeldRefusalHandoffCost/below_the_crossover_rate_the_grace_fires_first,_and_says_so
    refusal_bounds_integration_test.go:294
    expected: 1
    actual  : 0
    a client still draining when its grace ran out must be reported as the
    crossover, not left indistinguishable from one that stopped talking
```

It is **pre-existing**, not a regression from the OCI fixed-width decoder
(`2026-08-13-17`): the same subtest fails identically at `fb0a797`, the commit
before that work started, verified in a detached worktree on the same machine
and the same Docker daemon. The rest of the suite is green in both.

Everything before the failing line passes — the quota is held mid-stream
(`delta.held > 0`), the grace is what ends the session (`delta.watchdog > 0`),
and the byte bound does not fire (`delta.abandoned == 0`). So the session goes
exactly where the subtest says it should; what is missing is only the WARN that
names *why* (`logMsgRefusalGraceOutranBytes`). That record is gated by two
predicates in `session.go`, and it is one of them that is answering no:

- `heldRefusalWasStillReceiving(held, at)` — was the client still draining when
  the clock cut it;
- `refusalBoundOutOfReach(cost)` — `bound/(bytes/millis) > grace`, which needs
  `cost.bytes` and `cost.millis` both positive.

The first subtest logs an effective 80 KiB/s on a link set to 128 KiB/s, so the
throttled tap runs well under nominal on a loaded Docker host; a
`cost` that comes back at zero bytes or zero millis would silence the record
without changing anything else the subtest asserts. That is the first thing to
check, and it decides whether this is a test-environment sensitivity or a real
gap in the reporting.

## Implementation

1. Reproduce with `-run 'TestIntegration_HeldRefusalHandoffCost/below'` and log
   the `handoffCost` the crossover path computed, next to
   `heldRefusalWasStillReceiving`'s answer. Both are cheap to surface at DEBUG
   and neither is currently visible when the record is skipped, which is why the
   failure says nothing about its own cause.
2. If `cost.millis` or `cost.bytes` is zero: decide whether a hold cut by the
   grace with no measurable drain is a crossover (it is the same deployment
   shape) and make the predicate say so, rather than falling through to silence.
3. If both are positive and the arithmetic simply does not clear: the crossover
   rate has moved relative to `refusalHoldMaxBytes` / `refusalHandoffGrace`, and
   `crossoverLinkBytesPerSecond` in the test is the value to re-derive — see
   `docs/oracle.md`, "What a legitimate handoff costs, measured", whose numbers
   would then need re-taking too.
4. Whatever the answer, the subtest must fail with the measurement in the
   message; today it reports a count and leaves the reader to guess.

No GitHub issue filed (automation does not run `gh issue create` — see
`specs/todos/2026-08-11-06-*.md`); one should be filed by hand.
