# Two replicas sharing an instance id close each other's live connections

## Goal

Make the startup reconcile safe when several live processes carry the same
`DBB_INSTANCE_ID`, by distinguishing "this process" from "another process that
happens to share my instance id".

## Why

`Store.CloseOrphanedConnections` ([`internal/store/connections.go`](../../internal/store/connections.go))
has two branches. The reclaim branch is liveness-checked: it only touches
another instance's connections once the `instances` registry proves that
instance is gone. The own-instance branch is not — it closes every row with
`instance_id = <me> AND disconnected_at IS NULL`, unconditionally, because
"my id" was taken to mean "my previous run".

That assumption holds exactly as long as instance ids are unique per live
process. When two live replicas share one, a starting replica closes a live
peer's sessions — the precise failure the instance scoping exists to prevent:
the rows immediately satisfy the retention sweep's `disconnected_at < cutoff`
predicate, so `Store.CleanupOldQueryRows` can delete a connection row a live
session is still writing queries against, breaking the foreign key.

Two ways to get there:

- An operator sets an explicit `DBB_INSTANCE_ID` on a multi-replica Deployment —
  plausible, because before liveness tracking existed a stable id was the only
  way a restarting pod reclaimed its own orphans, and the docs recommended it.
  `internal/config/config.go` now warns against it, but a comment is not a
  guard.
- `config.FallbackInstanceID` (`"dbbat"`) is used whenever `os.Hostname()` fails.
  Every replica that hits it lands on the same id without anyone choosing it.
  It needs a broken container, but it is silent when it happens.

This predates the liveness work — the own-branch has always been unconditional.
Liveness tracking is what makes it fixable, and what makes leaving it a
documented hazard rather than an unavoidable one.

## Implementation

The own-branch cannot simply be liveness-checked: the reconciling process has
just registered its own id, so its row is fresh and the check would exclude
everything, reintroducing the leak this all started from. The identity needs to
be finer-grained than the instance id.

- Add a per-process **run id** (a UUID minted at startup, not configurable) and
  stamp it on connection rows alongside `instance_id`: a new nullable
  `connections.run_id` column plus a migration, mirroring
  `20260803020000_connections_instance_id`.
- Store it on the `instances` row too, so the registry identifies a *run* rather
  than an id. Consider making the registry key `(instance_id, run_id)`, which
  also lets two live replicas sharing an id both be represented — today the
  second one's upsert overwrites the first's heartbeat.
- Own-branch becomes `instance_id = <me> AND run_id <> <my run id>` — my id, not
  my run — and then the same liveness test as the reclaim branch applies to it
  uniformly: close it if no live run owns it. Rows with a NULL `run_id` (written
  before the column) keep the current unconditional behaviour or fold into the
  reclaim branch; decide deliberately, as with the empty `instance_id` case.
- Consider a startup warning when the registry already holds a fresh heartbeat
  for our instance id from a different run id: that is the shared-id
  configuration, and it is worth saying out loud even once it is safe.
- Tests in `internal/store/connections_test.go`: two live "replicas" sharing an
  instance id, one starting, the other's open connections untouched; plus the
  existing own-orphan case still reclaimed across a restart (same instance id,
  new run id).
- Once fixed, revisit the warnings in `internal/config/config.go`
  (`resolveInstanceID`, `FallbackInstanceID`) and the uniqueness note in
  `website/docs/configuration/index.md`.

No GitHub issue filed yet — one should be opened.

## Files

- `internal/store/connections.go` — `CloseOrphanedConnections`, the own-instance
  branch.
- `internal/store/instances.go` — the registry, keyed by instance id today.
- `internal/config/config.go` — `resolveInstanceID`, `FallbackInstanceID`.
- `internal/migrations/sql/` — the `run_id` column.
