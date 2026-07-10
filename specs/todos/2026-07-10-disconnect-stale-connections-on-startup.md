# Mark still-"active" connections as disconnected on startup

## Problem

The `connections` table tracks live client sessions: a row is inserted with
`CreateConnection` (`internal/store/connections.go:12`) and marked closed by
`CloseConnection` (`internal/store/connections.go:35`), which sets
`disconnected_at`. A connection is therefore considered **active** while
`disconnected_at IS NULL`.

If the process dies uncleanly — pod eviction, OOM kill, crash, `SIGKILL`, node
restart — `CloseConnection` never runs for the sessions that were open at the
time. Those rows keep `disconnected_at = NULL` forever, so they show up as
active connections even though the sockets died with the old process. After a
few restarts the active-connections view (`ListConnections`,
`internal/store/connections.go:88`, surfaced in the API/UI) accumulates ghost
sessions that can never be closed, because the in-memory proxy state that would
have called `CloseConnection` is gone.

On a fresh process start, **no** connection tracked by this instance can still
be alive — every socket belonged to the previous process. So any row left
`active` at startup is stale by definition and should be flipped to
disconnected.

## Proposal

On app/pod startup, sweep every still-active connection to disconnected before
the proxies begin accepting new clients.

1. **Store method** — add to `internal/store/connections.go`, e.g.
   `MarkAllConnectionsDisconnected(ctx) (int64, error)`:

   ```go
   result, err := s.db.NewUpdate().
       Model((*Connection)(nil)).
       Where("disconnected_at IS NULL").
       Set("disconnected_at = ?", time.Now()).
       Exec(ctx)
   ```

   Return `RowsAffected` so the caller can log how many stale rows were cleaned
   up. Mirror the pattern already in `CloseConnection`.

2. **Call site** — in `runServer` (`main.go:234`), after the store is
   initialized and migrations have run (`store.New`, `main.go:269`) and **before**
   the proxies start accepting connections (`startOracleProxy` /
   `startMySQLProxy` / the PG proxy), call the sweep and log the count, e.g.
   `logger.InfoContext(ctx, "Marked stale connections as disconnected", slog.Int64("count", n))`.
   Do it unconditionally on every startup — this must not be gated on test/demo
   mode.

3. **Ordering matters** — run the sweep before any listener is up so a client
   reconnecting immediately after restart creates a fresh row rather than racing
   the cleanup of its own new connection.

## Notes / open questions

- This is a single-instance assumption. If dbbat is ever run as **multiple
  replicas against the same storage DB**, a blanket "close everything on my
  startup" would wrongly disconnect sessions owned by sibling replicas. Today
  the deployment is a single pod (see the loadbalancer/deployment notes), so a
  global sweep is correct now; if we go multi-replica, scope the sweep by an
  instance/owner id recorded on each connection row instead.
- A `disconnected_at` set at startup is approximate (it reflects when the new
  process noticed, not when the old socket actually died). That's acceptable —
  it's strictly better than `NULL` forever. If precise close times matter later,
  consider using the previous process's last `last_activity_at` as the
  disconnect time instead of `now()`.
- Add a store unit test alongside `internal/store/connections_test.go`: insert a
  couple of open connections + one already-closed, run the sweep, assert the
  open ones now have `disconnected_at` set and the closed one is untouched.

## Implementation Plan

1. **Store method** (`internal/store/connections.go`) — add
   `MarkAllConnectionsDisconnected(ctx context.Context) (int64, error)`. Mirror
   `CloseConnection`: `NewUpdate()` on `(*Connection)(nil)`, `Where("disconnected_at
   IS NULL")`, `Set("disconnected_at = ?", now)`, then read `result.RowsAffected()`
   and return it. Unlike `CloseConnection`, a zero count is NOT an error (a clean
   startup with no stale rows is normal), so just return the count.

2. **Call site** (`main.go`, in `runServer`) — right after the store is up and
   migrations have run (after the "Database connection established" log at ~line
   276) and before ANY listener starts (API server, PG proxy, `startOracleProxy`,
   `startMySQLProxy`), call `dataStore.MarkAllConnectionsDisconnected(ctx)`,
   return-wrap any error (consistent with `EnsureDefaultAdmin` just below), and log
   the count with `logger.InfoContext(ctx, "Marked stale connections as
   disconnected", slog.Int64("count", n))`. Unconditional — not gated on
   test/demo mode.

3. **Store unit test** (`internal/store/connections_test.go`) — `TestMarkAllConnectionsDisconnected`:
   create two open connections and one already-closed (close it via
   `CloseConnection`, capturing its `disconnected_at`); call the sweep; assert it
   reports 2 rows affected; re-list and assert both open ones now have
   `disconnected_at` set and the already-closed one's timestamp is unchanged. Add a
   sub-test asserting a second sweep reports 0 rows affected (idempotent).

4. **QA** — `gofmt -w` the changed Go files, then `go build ./...`, `make lint`,
   `make test` until all green.
