# Upgrading from v0.20.x past the run-id migration breaks the old build's heartbeat

## Goal

Decide how much protection a v0.20.x replica deserves while
`20260804120000_connections_run_id_and_upstream_tls` is being rolled out, and say so somewhere an
operator will read it (release notes, `website/docs/configuration/index.md`, or
a longer grace period in code).

## Why

That migration makes `instances` primary key `(instance_id, run_id)`. A process
on v0.20.x — the only released build that knows the `instances` table, tagged
2026-08-04 — refreshes its row with `ON CONFLICT (instance_id) DO UPDATE`, and
that conflict target no longer exists, so from the instant the migration runs
its heartbeats fail with "there is no unique or exclusion constraint matching
the ON CONFLICT specification".

The row then stops moving. After `InstanceStaleAfter` (15 minutes) it looks dead
to the new build, which reclaims the connections that replica is still serving —
and a reclaimed row immediately satisfies the retention sweep's cutoff, so
`CleanupOldQueryRows` can delete a connection a live session is writing queries
against. The up-migration already refreshes every `run_id = ''` row's
`last_seen_at`, which buys a full grace period from the migration onwards, and a
rolling upgrade normally replaces such a process in far less than that. The
exposure is a rollout (or a rollback to v0.20.x) that leaves an old replica
serving for more than 15 minutes, with `replicaCount > 1`.

Nothing is broken today; this is about whether the window is documented or
closed.

## Implementation

Pick one, in increasing order of effort:

- Release note / docs only: "upgrade from v0.20.x completes within 15 minutes,
  and do not roll back to v0.20.x after migrating". Cheapest, and probably
  enough given v0.20.0 is a day old and `charts/dbbat/values.yaml` defaults to
  `replicaCount: 1`.
- Give registry rows with `run_id = ''` a longer grace period in
  `instanceStaleCutoff()` / `noLiveOwner()` (`internal/store/instances.go`,
  `internal/store/connections.go`). They can only come from a build that predates
  run tracking or from a migration seed, so a generous cutoff for them costs
  nothing once the fleet has run the new build once — but it delays reclaiming
  genuinely dead pre-upgrade rows by the same amount.
- Keep a compatibility registry: leave `instances` keyed by `instance_id` for
  old builds and put per-run rows in a sibling table, written by both. Fully
  rollback-safe in either direction, at the cost of two registries to explain
  and two upserts per heartbeat.

No GitHub issue filed yet — one should be opened.

## Files

- `internal/migrations/sql/20260804120000_connections_run_id_and_upstream_tls.up.sql` — the
  primary-key change and the `last_seen_at` refresh that mitigates it.
- `internal/store/instances.go` — `instanceStaleCutoff`, the upserts.
- `internal/store/connections.go` — `noLiveOwner`.
- `website/docs/configuration/index.md` — where an upgrade note would go.
