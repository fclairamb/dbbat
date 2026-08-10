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
