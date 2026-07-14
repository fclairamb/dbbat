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
