# The two held-refusal bounds cross at a link speed, and the slower one wins silently

## Goal

Decide whether `refusalHandoffGrace` should be a function of observed
throughput rather than a flat 30s, now that the two fail-safes are known to
constrain each other: below roughly 280 KiB/s of effective throughput the grace
runs out before `refusalHoldMaxBytes` can, so on a slow link the byte bound is
unreachable and a handoff that was going to succeed is cut by the clock instead.

## Why

`specs/done/2026/08/2026-08-13-11-size-the-held-refusal-bounds-against-a-measured-worst-case.md`
measured both bounds against live clients (see docs/oracle.md, "What a
legitimate handoff costs, measured"). Two of its numbers interact:

- a tail of 537 122 bytes took 6 541 ms over a 128 KiB/s tap — an effective
  80 KiB/s with the proxy and the tap in the path;
- `refusalHoldMaxBytes` is 8 MiB, which at that rate takes ~105s, three and a
  half times `refusalHandoffGrace`.

So the byte bound describes a case the grace cannot wait out. Neither value is
wrong on its own — both were kept deliberately — but nothing today notices the
crossover, and the operator-visible symptom is identical to the other three
fail-safes firing: an ORA-03113 where an ORA-00028 was expected.

This is speculative until someone reports it. Two things would settle it: a
deployment where clients sit behind a link slow enough to matter (dbbat is
usually next to the database, which is why this may never bite), and a decision
on whether "the client is still draining, just slowly" is distinguishable from
"the client stopped talking", which is what the grace is actually the fail-safe
for.

No GitHub issue yet — one should be filed.

## Implementation

Sketch, in increasing order of intrusiveness:

- **Report it and stop there.** `onLimitViolation`'s grace-expiry path already
  logs `relayed_bytes_since` and `held_for_ms` (`internal/proxy/oracle/session.go`).
  Add a WARN that names the crossover when the abandoned hold was still
  *receiving* — a hold that relayed a healthy number of bytes right up to the
  grace is a slow client, not a silent one, and saying so in the log is most of
  the operational value for none of the risk.
- **Make the grace conditional on progress.** Track the byte count at each
  `awaitRefusalHandoff` wake-up and extend the wait while it keeps climbing,
  capped by `refusalHoldMaxBytes`, which is then the single binding bound. That
  is the design that makes the two consistent: the grace stops being a deadline
  and becomes an idle timeout, which is what "the client stopped talking"
  always meant. The risk is that a trickling upstream then holds a session for
  as long as it takes to relay 8 MiB.
- Whichever is chosen, `TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut`
  (`internal/proxy/oracle/limits_test.go`) is the test that has to keep passing
  — its client sends nothing at all, which must still be cut at the grace — and
  the throttled tap in `internal/proxy/oracle/tap_test.go`
  (`startThrottledRecordingTap`) is the instrument for a new case where the
  client is slow rather than silent.

## Resolved open questions

> Decide whether `refusalHandoffGrace` should be a function of observed
> throughput rather than a flat 30s.

**Decision (2026-08-13): no — report the crossover, do not change the bound.**
The owner was asked directly during an `/implement-todos` batch and chose the
first sketch above ("Report it and stop there") over making the grace
progress-conditional.

Implement **only** the reporting option:

- Keep `refusalHandoffGrace` a flat 30s and keep `refusalHoldMaxBytes` at 8 MiB.
  Do **not** make the grace conditional on progress, do not turn it into an idle
  timeout, and do not touch `awaitRefusalHandoff`'s wait structure.
- On `onLimitViolation`'s grace-expiry path
  (`internal/proxy/oracle/session.go`), emit a WARN that names the crossover
  when the abandoned hold was **still receiving** — i.e. when the hold relayed a
  healthy number of bytes right up to the grace, which distinguishes a slow
  client from a silent one. Carry the existing `logAttrRelayedBytesSince` and
  `logAttrHeldForMillis` attributes (`internal/proxy/oracle/intercept.go:105`)
  so the log line says the handoff was cut by the clock while it was still
  making progress, and that the byte bound was therefore unreachable at that
  link speed.
- Behaviour is otherwise unchanged:
  `TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut` must keep passing
  untouched (its client sends nothing at all and must still be cut at the
  grace). Add a case using `startThrottledRecordingTap`
  (`internal/proxy/oracle/tap_test.go`) covering the slow-but-not-silent client,
  asserting the new WARN fires there and does **not** fire for the silent one.
- Record the rationale in the "What a legitimate handoff costs, measured"
  section of `docs/oracle.md`: the two bounds do cross below roughly 280 KiB/s,
  this is accepted deliberately because dbbat normally sits next to the
  database, and the WARN is what makes the case visible if a slow-link
  deployment ever meets it.
