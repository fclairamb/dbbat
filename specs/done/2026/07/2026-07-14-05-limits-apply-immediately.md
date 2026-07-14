---
model: opus
effort: high
---

# Enforce time/bandwidth grant limits mid-query, not only between commands

Originating issue: [#251](https://github.com/fclairamb/dbbat/issues/251)

## Problem
Grant limits (expiration time and `max_bytes_transferred` / `max_query_counts`)
are checked *before/after* each command rather than continuously. The checks
live at command boundaries — e.g. PostgreSQL `internal/proxy/postgresql/auth.go:152-156`,
Oracle `internal/proxy/oracle/intercept.go:561-565`, MySQL
`internal/proxy/mysql/auth.go:223-227`. A single long-running query can
therefore stream gigabytes for hours even with a "1h / 5MB" grant, because the
next boundary check never comes until the query finishes.

## Proposal
Enforce limits at the streaming level, aborting an in-flight query the moment a
limit is crossed:
- **Bandwidth**: track bytes transferred incrementally as result rows/packets
  flow through the proxy (the intercept/relay path already counts bytes for
  `BytesTransferred`), and when the running total crosses
  `MaxBytesTransferred`, terminate the current query/result stream and return a
  clear "quota exceeded" error to the client.
- **Expiration**: enforce `ExpiresAt` against wall-clock during streaming (e.g.
  a context deadline / periodic check on the relay loop), so a query that runs
  past the grant's expiry is cut off rather than allowed to complete.
- Apply consistently across PostgreSQL, Oracle, and MySQL (shared logic belongs
  in `internal/proxy/shared` where possible).
- Ensure partial results are terminated cleanly per each wire protocol
  (error/terminate frame) and that the grant's counters reflect the bytes
  actually sent.

Watch for concurrency correctness: byte counting and limit checks run on the
data path and must be cheap and race-free.

## Implementation Plan

### Current state (analysis)
- All three protocols already funnel client-side traffic through a shared
  `shared.CountingConn` that atomically accumulates `bytesFromClient` /
  `bytesToClient`. The running total is available at any instant, mid-stream.
- Limits are only checked at command boundaries: each protocol has a
  `checkQuotas()` invoked before a *new* query runs (PG `handleQuery`/`handleExecute`,
  Oracle `interceptClientMessage`, MySQL `runIntercepted`). Those checks read
  the per-query-updated `grant.BytesTransferred` / `grant.QueryCount`.
- Grant **expiration** is worse: it is only enforced once, at connect time
  (`store.GetActiveGrant` filters `expires_at > NOW()`). Nothing re-checks
  `ExpiresAt` afterward — not even between commands.

### Step 1 — Shared `LimitGuard` (`internal/proxy/shared/limits.go`)
A cheap, race-free, protocol-agnostic guard:
- Constructed from a `*store.Grant` plus the two live atomic byte counters.
  Snapshots `baseBytes = grant.BytesTransferred` (already-consumed before this
  session), `maxBytes = grant.MaxBytesTransferred`, `expiresAt = grant.ExpiresAt`.
- `Check() error` → `ErrByteQuotaExceeded` when
  `baseBytes + bytesFromClient + bytesToClient >= *maxBytes`, or
  `ErrGrantExpired` when `now() >= expiresAt`; nil otherwise (nil grant / unset
  limits never trip). Two atomic loads + a time compare — safe to call on the
  data path.
- `Watch(ctx, interval, onViolation)` — polls `Check()` on a ticker (default
  250ms) and fires `onViolation(err)` exactly once when a limit is first
  crossed, then returns; also returns on `ctx.Done()`. Injectable clock
  (`now`) for deterministic tests.
- New sentinel errors `ErrByteQuotaExceeded`, `ErrGrantExpired`.

### Step 2 — Per-session watchdog (guaranteed teardown, all three protocols)
Each session builds a `guard` once the grant + upstream conn exist, then starts
`go guard.Watch(wctx, …, onLimitViolation)` with a `context.WithCancel(session ctx)`
cancelled on session exit (no goroutine leak). `onLimitViolation` logs and
force-closes both the client and upstream conns, which unblocks any relay
goroutine or blocked `Execute` and tears the session down. This is the
consistent cross-protocol mechanism and the only viable one for MySQL (the
go-mysql library owns the wire and buffers whole results) and for the idle-time
case (a query blocked producing no traffic while the deadline passes).

### Step 3 — Inline clean-frame termination for the streaming relays (PG + Oracle)
Where dbbat itself drives the wire and can hit a message boundary, emit a real
protocol error instead of a bare reset:
- **PostgreSQL** `proxyUpstreamToClient`: after forwarding each result message,
  while a query is in flight, call `guard.Check()`; on violation send
  `ErrorResponse` + `ReadyForQuery` and return (→ cleanup closes upstream, server
  closes client).
- **Oracle** `upstreamToClient`: after forwarding a Data packet, call
  `guard.Check()`; on violation send a TTC error frame (`writeTTCError`,
  ORA-00028 "session terminated") and return.

### Step 4 — Close the between-commands expiry gap (all three)
Add an `ExpiresAt` check to each boundary `checkQuotas` path (reusing
`shared.ErrGrantExpired`) so a command issued after the grant expires is
rejected cleanly, not just mid-stream.

### Step 5 — Tests
- `shared/limits_test.go`: `Check()` (byte overage, expiry, unset/nil grant),
  and `Watch()` firing `onViolation` for both byte and time violations and
  stopping on ctx cancel (injected clock).
- Per-protocol wiring tests: guard built from grant; boundary `checkQuotas`
  rejects expired grants; the streaming abort path (PG/Oracle) sends the right
  frame. MySQL: watchdog `onViolation` closes conns.
- Integration-style relay test (PG via `net.Pipe`) asserting a mid-stream byte
  cap produces an `ErrorResponse` and terminates.
