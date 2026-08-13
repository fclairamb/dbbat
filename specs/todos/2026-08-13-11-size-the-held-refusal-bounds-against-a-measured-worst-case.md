# Size the two held-refusal bounds against a measured worst case

## Goal

Replace the reasoned values of `refusalHoldMaxBytes` (8 MiB) and
`refusalHandoffGrace` (30s) — `internal/proxy/oracle/session.go` — with values
backed by a measurement of what a legitimate handoff actually costs, or record
why the reasoned ones are right and stop calling them provisional.

## Why

Spec `2026-08-13-07-measure-the-held-mid-reply-refusal-on-the-other-two-clients`
closed the coverage gap on these two fail-safes: both now fire end to end
through the real relay and the real watchdog
(`TestUpstreamToClient_StopsRelayingPastTheOvershootBound`,
`TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut`), reachable because the
bounds became per-session overrides (`refusalHoldBytes` / `refusalHoldGrace`).

What that did **not** settle is whether 8 MiB and 30s are the right numbers.
They were added on the implementing spec's own judgement — the spec asked for a
boundary, not for bounds on waiting for one — and they are the difference
between the worst case being ≤30s/≤8 MiB and being the ~250ms the watchdog used
to give. Every client measured on the delivered path is *orders of magnitude*
inside both: the five clients in docs/oracle.md all announced their boundary
within one fetch batch and within milliseconds on loopback. So the bounds may be
generous by three orders of magnitude, which is a real (if bounded) enforcement
window nobody has justified.

No GitHub issue yet — one should be filed.

## Implementation

- Instrument the delivered path: log, or better assert, the actual
  `cumulativeClientBytes() - held.atBytes` and `time.Since(held.armedAt)` at
  `answerHeldRefusal`, and collect them across the five clients the integration
  suites already drive (`TestIntegration_AsyncRefusalAgainstJDBCThin`,
  `TestIntegration_AsyncRefusalAgainstOCIAndPythonThin`). That is the observed
  cost of a legitimate handoff.
- The interesting case is the one none of those exercise: a **large fetch size**
  (the cost is "the tail of one fetch batch, bounded by the client's fetch
  size"), and a client on a slow link rather than loopback. A run with
  `arraysize`/`setFetchSize` at the driver maximum bounds the byte side
  honestly; the time side needs a deliberately throttled tap.
- Then either narrow the constants to a measured multiple of that, or write the
  measured numbers into docs/oracle.md next to the fail-safe table and drop the
  "reasoned, not measured" caveat.
- Related: the values may want to be operator-visible rather than compile-time,
  in which case the per-session override fields are already the seam.
