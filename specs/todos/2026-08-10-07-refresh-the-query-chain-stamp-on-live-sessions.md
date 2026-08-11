---
model: opus
effort: high
---

# Refresh the query chain stamp while a session is still open

## Goal

Keep `connections.query_chain_mac` / `query_chain_len` roughly current for
**open** sessions instead of only writing them at close, so the window in which
a session's tail is unprotected is bounded by a refresh interval rather than by
how long the session lasts — or, for a crashed one, by how long the reconcile
takes to notice.

## Why

The stamp is the only thing that detects statements deleted from the **end** of
a session. Today it is written at exactly two moments:

- `Store.CloseConnection` — a clean teardown;
- `Store.closeOrphans` / `Store.stampOrphanHeads` — the reconcile that closes
  crash orphans (`2026-08-10-stamp-chain-head-on-orphaned-connections.md`).

Both are *close* events. So:

- a **long-lived open session** — a psql window left open all day, a pooled
  application connection, an approval hold waiting on a human — has no stamp at
  all for its whole life. Deleting its most recent statements right now is
  undetectable, and stays undetectable until it closes, at which point the close
  seals the already-truncated chain;
- a **crashed** session is unprotected from the crash until the reclaim runs.
  That is up to `InstanceStaleAfter` (15min) plus `InstanceReclaimInterval`
  (~7.5min) for a process killed outright.

Neither window is exotic on a rescheduled pod, and both shrink to the refresh
interval with the same mechanism.

No GitHub issue yet — file one when picking this up.

## Implementation

- The head is already recoverable from the database alone:
  `queryChainHeadSelect` (`internal/store/queries.go`) is the shared builder,
  and `Store.orphanHeadSelect` (`internal/store/connections.go`) shows the
  `LEFT JOIN LATERAL` shape that reads many heads at once without any
  in-process state.
- Cheapest shape: fold the stamp into a periodic sweep over open connections —
  select the open uids this run owns, read their heads with `orphanHeadSelect`,
  seal in Go and write them back in one bulk `UPDATE`, run on the existing
  reconcile timer. That is `Store.stampOrphanHeads` with a different uid source.
  It costs one index lookup per open connection per pass, so it scales with
  concurrency, not with the store; measure it the way
  `TestQueryChainOrphanStampCostScalesWithOrphans` measures the reconcile.
  Restricting it to rows this run owns (`run_id = s.runID`) keeps replicas from
  fighting over the same rows.
- Alternative worth pricing: stamp from the append path itself, every N
  statements or on the connection's activity update. Tighter window, but it puts
  a second write on the hot path.
- `checkStampedHead` (`internal/store/chain_verify.go`) needs no change for the
  refresh itself — a stamp that is a few statements behind the surviving chain
  must **not** be reported as a break for an open session, so the comparison has
  to become "the stamp is a prefix of what survives" while
  `disconnected_at IS NULL`, and stay exact once the session is closed. That is
  the substantive design work here, and it is why this is not a one-liner.
- The keyed stamp landed with
  `2026-08-10-06-seal-the-connection-query-chain-stamp.md`, so a refresh sweep
  is already select-seal-write in Go — the reconcile's plumbing
  (`stampOrphanHeads`, `orphanHeadSelect`, `queryChainStampMAC`) is there to be
  reused, and a refresh must write `query_chain_stamp_version = 1` like the
  other two writers.

## Implementation Plan

### 1. Share the seal-and-write half (`internal/store/connections.go`)

`stampOrphanHeads` becomes a thin wrapper over a `stampChainHeads(ctx, db
bun.IDB, uids, guard)` that does the existing select-seal-write and returns how
many rows it stamped. Two things change while it is being generalised:

- it takes `bun.IDB` rather than `bun.Tx`, so the refresh can run it outside a
  transaction (there is no close to keep atomic with it);
- it takes a `guard` predicate ANDed onto the write. The reconcile passes none;
  the refresh passes `c.disconnected_at IS NULL`, which is what stops a refresh
  that read a head at seq 5 from landing *after* a concurrent `CloseConnection`
  wrote the exact final stamp at seq 20 and leaving the closed row sealed at a
  head it no longer ends on. Under READ COMMITTED the losing `UPDATE`
  re-evaluates the predicate after it gets the row lock, so the guard holds in
  both interleavings;
- it batches the VALUES list (`chainStampBatchSize`), because a busy replica can
  have more open sessions than PostgreSQL will take bind parameters for.

### 2. The sweep (`internal/store/connections.go`)

`Store.RefreshOpenChainStamps(ctx) (int64, error)`: select `uid` from
`connections` where `disconnected_at IS NULL AND run_id = s.runID`, then hand
those uids to `stampChainHeads` with the open guard. Restricted to this run's
rows so replicas never contend over the same rows, and a no-op when chaining is
off or the run id is empty.

### 3. Fold it into the reconcile timer (`heartbeat.go`)

The reclaim tick already fires every `store.InstanceReclaimInterval` (~7.5min,
spread over [half, 1.5×]). `h.refreshOpenStamps(ctx)` rides along on it, logged
at debug on success and warn on failure — the next tick retries and nothing
about serving traffic depends on it.

### 4. Prefix-vs-exact verification (`internal/store/chain_verify.go`)

This is the substantive half. `checkStampedHead` also reads `disconnected_at`,
and the rule forks on it. `query_chain_len` is the head's `chain_seq` at stamp
time, so a stamp left by an earlier sweep names an *older, smaller* `chain_seq`
and the MAC over the statement at that position — not over the current head.

For a **closed** session nothing changes: the stamp must name exactly
`result.HeadSeq` and seal exactly `result.HeadMAC`.

For an **open** session with a keyed stamp:

| stamped `chain_seq` vs the surviving chain | verdict |
|---|---|
| equal to `HeadSeq` | exact compare, as today |
| below `HeadSeq`, and that statement survives | re-read *that* statement's MAC and check the stamp against it — the stamp is a prefix, the session simply moved on |
| below the oldest surviving statement | `DBB_QUERY_STORAGE_RETENTION` reaped the sealed point; unverifiable, already counted as a truncated prefix, **not** a break |
| above `HeadSeq` | **break** — retention only ever removes the oldest, so nothing legitimate makes the stamp outrun the survivors |
| nothing survives at all | unverifiable for an open session (retention can reap every statement of an idle long-lived session); a closed one keeps today's break |

`QueryChainResult` grows a `FirstSeq` field (the oldest surviving `chain_seq`),
set by the walk, which is what tells "reaped below the stamp" from "hole inside
the surviving range".

The honest limit, to be written into `docs/audit-chain.md`: the stamp only ever
proves the chain up to the position it sealed, so on an open session the
unprotected tail is bounded by the sweep interval instead of by the session's
lifetime. Deleting statements newer than the last sweep is still undetectable —
that window is what this spec shrinks, not closes.

### 5. Tests (`internal/store/chain_test.go`)

- a refresh stamps an open session, and the session is still open afterwards;
- an open session that is merely **behind** (statements appended after the
  sweep) verifies clean;
- an open session whose tail was **deleted below the stamp** is a break;
- an open session whose stamp is rewritten/re-sealed at the truncated head
  without the key is a break (the keyed stamp still does its job while open);
- a **closed** session stays exact — a behind stamp on a closed row is a break;
- retention reaping past the stamped position on an open session is not a break;
- the sweep only touches rows this run owns;
- `TestQueryChainRefreshCostScalesWithOpenSessions`: the uid select never reads
  `queries`, and the head lookup runs once per open connection *this run owns* —
  not once per connection in the store, mirroring
  `TestQueryChainOrphanStampCostScalesWithOrphans`.

### 6. Docs

`docs/audit-chain.md` gains the third writer and the prefix rule;
`CLAUDE.md`'s audit-trail bullet gains the sweep.
