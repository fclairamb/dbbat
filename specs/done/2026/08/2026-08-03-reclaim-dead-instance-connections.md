# Connections owned by an instance that never comes back are still never reaped

## Goal

Let a dbbat process reclaim the crash-orphaned connections of *other* instances
that are provably gone, so the residue left behind by
[`2026-08-03-reap-crash-orphaned-connections.md`](2026-08-03-reap-crash-orphaned-connections.md)
does not accumulate forever.

## Why

`Store.CloseOrphanedConnections` ([`internal/store/connections.go`](../../internal/store/connections.go))
closes the connections a previous run left open, but only those stamped with
*this* process's `instance_id`. That scoping is not optional: dbbat runs several
replicas against one store (see [`docs/approvals.md`](../../docs/approvals.md),
"Multiple replicas"), and a blanket `UPDATE ... WHERE disconnected_at IS NULL`
would let a starting replica close another replica's **live** connections —
which then satisfy the retention sweep's `disconnected_at < cutoff` predicate,
so `Store.CleanupOldQueryRows` could delete a connection row a live session is
still logging queries against.

The cost of that safety is that identity alone cannot tell "gone" from "busy".
Two kinds of row are therefore never reclaimed:

- Rows owned by an instance id that never returns. The default id is the
  hostname, so a single host, a StatefulSet, or an explicit `DBB_INSTANCE_ID`
  all recover their own orphans on restart — but a plain Kubernetes Deployment
  mints a new pod name every time, so a replacement pod does not recognize its
  predecessor's rows.
- Rows created before the `instance_id` column existed (migration
  `20260803020000_connections_instance_id`), which carry `''`.

Both keep counting as "currently connected" in the UI and in anything filtering
on `disconnected_at IS NULL`, and both survive every retention sweep.

## Implementation

Replace "is this row mine?" with "is this row's owner alive?" — liveness, not
identity.

- Add an `instances` table (`instance_id` PK, `started_at`, `last_seen_at`).
  Each process upserts its row at startup and refreshes `last_seen_at` on a
  ticker (reuse the cadence style of `queryHistorySweepInterval` in
  [`retention.go`](../../retention.go), but much shorter — tens of seconds).
- On clean shutdown, delete the row. That makes the common case immediate rather
  than waiting out a timeout.
- Extend the startup reconcile: besides its own rows, close connections whose
  `instance_id` has no `instances` row, or whose `last_seen_at` is older than a
  generous multiple of the heartbeat interval (a stale-instance grace period).
  Keep `disconnected_at = last_activity_at`.
- Decide explicitly what to do with legacy `instance_id = ''` rows. They have no
  owner and never will; a one-shot reconcile behind an opt-in flag, or simply
  folding them into the "no instances row" case, are both defensible — but the
  choice must be a deliberate, documented one, not a side effect.
- Log reclaimed-from-another-instance counts separately from own-instance
  counts: the former is a signal that a pod died without shutting down.
- Tests in [`internal/store/connections_test.go`](../../internal/store/connections_test.go):
  a live (recently heartbeating) instance's open connections are never touched;
  a stale instance's are; a deregistered instance's are; the timestamp is still
  `last_activity_at`.
- Update the `DBB_INSTANCE_ID` notes in [`CLAUDE.md`](../../CLAUDE.md),
  [`README.md`](../../README.md) and
  [`website/docs/configuration/index.md`](../../website/docs/configuration/index.md),
  which currently document the un-reclaimed residue as a known limitation.

No GitHub issue filed yet — one should be opened.

## Files

- `internal/store/connections.go` — `CloseOrphanedConnections`, to be widened
  from identity to liveness.
- `internal/migrations/sql/` — the new `instances` table.
- `main.go` — `reconcileOrphanedConnections`, plus registering/deregistering
  this instance around the server lifecycle.
- `internal/store/queries.go` — `CleanupOldQueryRows`, the sweep this unblocks.
