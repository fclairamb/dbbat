# Persist bytes of a query aborted by a mid-stream grant limit

## Goal
When a running query is cut off mid-stream because the grant's byte quota or
expiry was crossed (see `2026-07-14-05-limits-apply-immediately.md`), record the
bytes that were actually streamed for that aborted query, so the grant's
cumulative `BytesTransferred` reflects them across reconnects.

## Why
The mid-stream enforcement added in spec 05 tears the session down the moment a
limit is crossed. But bytes are only persisted (`IncrementConnectionStats` +
query log row) when a query *completes normally* — PostgreSQL on `ReadyForQuery`,
Oracle on the OER/end-of-data marker, MySQL after `Execute` returns. An aborted
query never reaches that point, so its streamed bytes are counted in the live
`CountingConn` atomics (and correctly enforced *within* the session) but are
never written to the store.

Consequence: on a fresh reconnect, `store.GetActiveGrant` recomputes
`BytesTransferred` from persisted data, which is missing the aborted query's
bytes. A user could therefore reconnect and receive a full `MaxBytesTransferred`
budget again each time, partially bypassing the cumulative cap across many
short-lived connections. (This mirrors a pre-existing, narrower undercount noted
in `internal/proxy/mysql/session.go` `recordDisconnect`.)

## Implementation
- On the abort path, before/while tearing down, attribute the in-flight query's
  bytes to the grant:
  - PostgreSQL: in `abortStream` / `enforceStreamLimits`
    (`internal/proxy/postgresql/session.go`), snapshot
    `bytesFromClient+bytesToClient - lastBytesSnapshot` and call
    `logQuery(...)` (or at least `IncrementConnectionStats`) for the aborted
    `currentQuery` with an error like "aborted: grant limit reached".
  - Oracle: in the `upstreamToClient` abort branch
    (`internal/proxy/oracle/session.go`), finalize `tracker.pendingQuery` via
    `completeQuery(nil, &errMsg)` so its bytes are flushed before returning.
  - MySQL: the watchdog closes conns mid-`Execute`, so the handler's
    `recordQuery` never runs. Consider a bytes-only increment on teardown
    (needs an `IncrementConnectionBytes` store helper that does not also bump
    the query count — see the existing `recordDisconnect` comment).
- Add a store helper to increment only `bytes_transferred` if the query-count
  coupling in `IncrementConnectionStats` is in the way.
- Tests: assert that after a mid-stream abort, the connection/grant byte total
  includes the streamed-so-far bytes.

No GitHub issue yet — file one when this is picked up (relates to
[#251](https://github.com/fclairamb/dbbat/issues/251)).

## Implementation Plan

### Store helper (`internal/store/connections.go`)
- Add `IncrementConnectionBytes(ctx, uid, bytes)` next to
  `IncrementConnectionStats`. Same UPDATE but WITHOUT `queries = queries + 1`:
  only `bytes_transferred = bytes_transferred + ?` and `last_activity_at`. Used
  by the MySQL teardown flush (no query context to log a row against) and is the
  bytes-only primitive the spec calls for.
- Tests (`internal/store/connections_test.go`, `grants_test.go`):
  `TestIncrementConnectionBytes` asserts bytes bump but query count unchanged;
  a grants test asserts `populateGrantCounters`/`GetActiveGrant` recompute picks
  up bytes flushed this way (the core reconnect-bypass scenario).

### PostgreSQL (`internal/proxy/postgresql/session.go` + `intercept.go`)
- `abortStream(cause)` (called from `enforceStreamLimits`) gains a final
  `s.persistAbortedQuery(cause)` after the error frame is written.
- New `persistAbortedQuery`: `query := getCurrentPendingQuery()`; snapshot
  `bytesFromClient+bytesToClient - lastBytesSnapshot`, advance `lastBytesSnapshot`,
  skip if `<= 0`. Promote the in-flight query to `s.currentQuery` (Extended Query
  Protocol leaves it in the pending queue) and call
  `logQuery(nil, &"aborted: <cause>", delta)` — logs a failed query row +
  `IncrementConnectionStats` + in-memory grant bump.
- `logQuery` gains a `s.store != nil && s.connectionUID != uuid.Nil` guard around
  its async store write (keeps the in-memory grant update always; prevents nil
  panics on the abort path when no connection record exists).
- No double-count: `enforceStreamLimits` returns early when
  `getCurrentPendingQuery() == nil`, so a normally-completed query (currentQuery
  nil after `ReadyForQuery`) never hits the abort path; the relay returns right
  after abort so the real `ReadyForQuery` is never processed.
- Test: extend `TestSession_ProxyUpstreamToClient_ByteLimitAbort` to assert
  `grant.BytesTransferred >= maxBytes` and `lastBytesSnapshot` advanced after the
  mid-stream abort (store nil → in-memory attribution).

### Oracle (`internal/proxy/oracle/session.go`)
- In `upstreamToClient`'s abort branch (`if s.tracker.pendingQuery != nil` →
  `guard.Check()` trips), after `writeTTCError`, call
  `s.completeQuery(nil, &"aborted: <cause>")` before `return verr`.
  `completeQuery` already diffs `cumulativeClientBytes - lastBytesSnapshot`,
  persists (create + `IncrementConnectionStats`, guarded by `s.store != nil`),
  bumps the in-memory grant, and nils `pendingQuery` (no double-count).
- Test: extend `TestUpstreamToClient_ByteLimitAbort` to assert
  `grant.BytesTransferred > 0` after the abort.

### MySQL (`internal/proxy/mysql/session.go` + `intercept.go`)
- The result is buffered and written to the client only after `Execute`/
  `recordQuery`; on a mid-`Execute` teardown the last query's trailing response
  (and any aborted-request) client bytes are never persisted. `recordDisconnect`
  now flushes them: `delta := cumulativeClientBytes() - lastBytesSnapshot`; when
  `> 0`, `IncrementConnectionBytes(connection.UID, delta)` (bytes only — no
  spurious query row/count). Replaces the "out of scope" comment.
- No double-count: `recordQuery` keeps `lastBytesSnapshot` current for every
  recorded query, so the teardown delta excludes already-persisted bytes.
- Test: PG-store-container-backed `TestRecordDisconnect_FlushesUnrecordedBytes`
  — connection with a stale `lastBytesSnapshot`, call `recordDisconnect`, assert
  `bytes_transferred` grew by the delta and `queries` unchanged.
