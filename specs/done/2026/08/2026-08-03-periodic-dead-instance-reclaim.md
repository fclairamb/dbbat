# Dead-instance connections are only reclaimed when some process restarts

## Goal

Reclaim the connections of a dead instance without waiting for an unrelated
process to start, so a crash-and-restart cycle does not leave rows open for as
long as the deployment happens to stay stable.

## Why

`Store.CloseOrphanedConnections` ([`internal/store/connections.go`](../../internal/store/connections.go))
now closes connections whose owning instance has no `instances` row or has not
heartbeated for `store.InstanceStaleAfter` (15 minutes) — but it only runs once,
at startup, from `reconcileOrphanedConnections` in [`main.go`](../../main.go).

That leaves a gap in the most common crash shape. A pod is `SIGKILL`ed; its
registry row is left behind with a `last_seen_at` of a few seconds ago. The
replacement pod starts immediately, sees a row that is well inside the grace
period, and reclaims nothing. By the time the row goes stale — 15 minutes later
— nothing is starting any more, so the dead pod's connections stay open until
the *next* restart of some instance, which may be days away. On a stable
deployment the residue is exactly as long-lived as before.

A clean shutdown does not have this problem (the row is deleted, so the
replacement reclaims immediately); it is specifically the crash path, which is
the path this whole feature exists for.

## Implementation

- Run the foreign-instance half of the reconcile on a ticker as well as at
  startup. The predicate is unchanged, so the safety argument is unchanged: a
  live instance is one that heartbeated within `InstanceStaleAfter`.
- Cheapest wiring is to piggyback on the existing heartbeat loop in
  [`heartbeat.go`](../../heartbeat.go) at a lower frequency (say every
  `InstanceStaleAfter/2`), or to add a second ticker next to it. Reuse
  `Store.PruneStaleInstances` on the same cadence.
- Split `CloseOrphanedConnections` so the reclaim half can be called on its own
  without redoing the own-instance update (which is startup-only by nature).
- Log the reclaimed count the same way `reconcileOrphanedConnections` does — a
  non-zero count away from startup is still "a process died without shutting
  down".
- Guard against every replica doing the work at once: it is idempotent (the
  second `UPDATE` matches nothing) but a `SELECT ... FOR UPDATE SKIP LOCKED` or
  simply staggering the ticker start would keep the churn down.
- Tests: extend `internal/store/connections_test.go` with a reclaim that runs
  without a preceding registration, and cover that a live instance is still
  never touched.

No GitHub issue filed yet — one should be opened.

## Files

- `internal/store/connections.go` — `CloseOrphanedConnections`.
- `heartbeat.go` — the existing per-process ticker.
- `main.go` — `reconcileOrphanedConnections`.
