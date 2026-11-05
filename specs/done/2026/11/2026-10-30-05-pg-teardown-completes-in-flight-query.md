---
model: opus
effort: medium
---

# PostgreSQL: a client disconnect mid-result skips the byte quota and leaves the query open

## Problem

Found by the security scan of 2026-10-30 (finding M5).

Severity: Medium. Triggered by any authenticated user, and it defeats `max_bytes_transferred` for users who have that quota.

Bytes reach the store only when a query completes: `logQuery` (`internal/proxy/postgresql/intercept.go:615`, `IncrementConnectionStats` at `intercept.go:754`) runs at `ReadyForQuery`. The abort and termination paths attribute the in-flight bytes (`persistAbortedQuery` `session.go:1501`, `persistTerminatedQuery` `session.go:1552`). A plain client disconnect takes neither path. `proxyMessages` returns on the first leg that ends (`session.go:540`), and `cleanup` (`session.go:1625`) closes the connection row without flushing bytes or completing the in-flight statement.

Reproduced on 2026-10-30 through the proxy: a client that reads part of a large result and closes its socket leaves a `queries` row with no duration and no completion. The streamed bytes are never added to the connection. On reconnect the grant's `BytesTransferred` is recomputed from the store without them. A user can stream just under the remaining byte budget, disconnect, reconnect and repeat, exporting any multiple of the cap. The MySQL and SQL Server proxies flush trailing bytes at teardown (`internal/proxy/mysql/session.go:497-505`, `internal/proxy/mssql/session.go:683-689`); PostgreSQL does not.

## Proposal

1. In `cleanup` (or at the end of `proxyMessages`, under `bookMu`), before `CloseConnectionWithReason`:
   - complete every statement still in flight (`currentQuery` and the pending queue), with an error such as "client disconnected before the response completed", reusing the `persistTerminatedQuery` shape;
   - flush `bytesFromClient + bytesToClient - lastBytesSnapshot` to the connection, with `IncrementConnectionBytes` if no statement takes it.
2. Make sure this runs after both relay goroutines are done touching `bookMu` state, or under the lock, to stay race-free.

## Tests

- Integration: with `max_bytes_transferred` set, a client reads part of a large result and disconnects. After reconnecting, the grant's byte counter includes the bytes already streamed, and the statement's row is completed with the disconnect error. This fails before the fix.
- Positive control: a normal session that ends with `Terminate` after its queries completed adds no extra bytes and leaves no extra error rows.
- `go test -race` on the package.
