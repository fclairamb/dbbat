# The integration suites run without the race detector

## Goal

Decide whether `-race` belongs on the protocol integration targets in the
`Makefile` (`test-e2e-oracle`, `test-integration-{mongodb,mysql,postgresql,mssql,kubernetes}`)
and on `.github/workflows/integration.yml`, and if so, turn it on and fix what
it finds.

## Why

`make test` runs `go test -race ./...`; every integration target runs plain
`go test -tags integration`. So the only suites that put two proxy goroutines on
the same session at the same time — the client reader, the upstream reader, the
limit watchdog, the approval gate — are the ones running *without* the detector.
Unit tests hold the detector but drive one goroutine.

That gap already hid a real bug. `2026-08-10-17-oracle-refusal-frame-hangs-the-client`
added `session.oer`, written by the pre-auth pump goroutine (the Set Protocol
reply) and by the relay's main loop (the client's Set Data Types) at the same
time, with `min(existing, observed)` on both sides — a read-modify-write, not a
torn word. Every non-pipelined client overlaps them. The Oracle suite ran green
across it, twice, because it carries no detector; it was found by review.

Turning the detector on for one Oracle test immediately found two more, both
pre-dating that work and both on `session.tracker.pendingQuery`:

```
Write  intercept.go:426  (*session).handlePiggybackExec   [client reader]
Read   session.go:1412   (*session).upstreamToClient      [upstream reader]

Write  intercept.go:856  (*session).completeQuery         [client reader]
Read   session.go:1412   (*session).upstreamToClient      [upstream reader]
```

`session.go:1412` is the `if s.tracker.pendingQuery != nil` guard that gates the
mid-stream limit check, so the read happens once per forwarded packet while the
client goroutine is setting and clearing the pointer. That is why
`go test -race -tags integration ./internal/proxy/oracle/ -run TestIntegration_BlockedStatementRefusesSQLPlus`
fails today even though the suite is green without the detector — and why
`-race` cannot simply be added to the Makefile target until these are fixed.

No GitHub issue yet — file one when picking this up.

## Implementation

- Cost first: `-race` is roughly a 2-10x CPU tax, and these suites are already
  dominated by container startup rather than CPU (the Oracle suite is ~5-7min
  wall for ~30s of actual test work). Measure one target before and after
  rather than assuming either way.
- Start with `test-e2e-oracle` alone — it has the most shared-session state and
  the most client shapes — and only widen once it is green.
- Expect findings around `internal/proxy/oracle/session.go`: `tracker`
  (`learnCursorID` vs `gateStatement`), `tracker.pendingQuery`
  (`upstreamToClient` vs `interceptClientMessage`), and `lastBytesSnapshot`.
  The fix pattern is already in the file — `heldMu` and `oerMu` are per-field
  mutexes on exactly this kind of state.
- If the tax turns out to be unacceptable on the slower images (the 18c XE
  matrix leg, the MSSQL amd64-under-emulation leg), consider a `-race` run of
  only the fast subset in CI rather than dropping it entirely: the value is in
  covering the concurrent proxy paths at all, not in covering every test.
