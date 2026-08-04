# Connections that were never closed are never reaped

## Goal

Give `connections` rows whose `disconnected_at` was never set a way out, so the
query-history retention sweep can eventually remove them instead of leaving
them behind forever.

## Why

The retention sweep added in
[`2026-08-03-query-rows-retention.md`](2026-08-03-query-rows-retention.md)
(`Store.CleanupOldQueryRows`, [`internal/store/queries.go`](../../internal/store/queries.go))
deliberately only reaps connections with `disconnected_at IS NOT NULL AND
disconnected_at < cutoff`. Deleting a connection that is still open would break
the foreign key for the next query that session logs, so "still open" is treated
as "keep".

But `disconnected_at` is only ever written by `Store.CloseConnection`
([`internal/store/connections.go:36`](../../internal/store/connections.go)), which
runs on a clean session teardown. If dbbat is killed, crashes, or the pod is
rescheduled mid-session, every live connection keeps `disconnected_at = NULL`
forever. Those rows then:

- survive every retention sweep, however old they get;
- outlive all of their queries (those *are* reaped by timestamp), so the UI
  shows a connection with a non-zero `queries` counter and nothing to list;
- count as "currently connected" anywhere that filters on
  `disconnected_at IS NULL`.

## Implementation

Reconcile at startup rather than loosening the sweep predicate — the sweep's
rule is safe precisely because it does not guess whether a session is live.

- On server start (in `runServer`, [`main.go`](../../main.go), before the proxies
  begin accepting), mark every connection still flagged open as disconnected:
  `UPDATE connections SET disconnected_at = last_activity_at WHERE
  disconnected_at IS NULL`. Nothing that predates this process can still be
  live — the sessions died with the previous process.
- Use `last_activity_at` (not `NOW()`) as the timestamp so retention measures
  from when the session actually stopped, and a crashed session doesn't get its
  clock reset on every restart.
- Log the count at info level; a large number is a useful crash signal.
- Guard the single-writer assumption: this is only correct if one dbbat process
  owns the store. If multiple replicas ever share a store, scope the update by
  an instance id instead of blanket-updating. Note the assumption in the code
  comment either way.
- Test in `internal/store/connections_test.go`: an open connection is closed at
  `last_activity_at`, an already-closed one keeps its original timestamp, and a
  reconciled connection is then reaped by `CleanupOldQueryRows` once past the
  cutoff.

No GitHub issue filed yet — one should be opened.

## Files

- `internal/store/connections.go` — the reconcile query (e.g.
  `CloseOrphanedConnections`).
- `main.go` — call it during `runServer`, before proxies start.
- `internal/store/queries.go` — `CleanupOldQueryRows`, whose predicate this
  makes reachable for crash-orphaned rows.
