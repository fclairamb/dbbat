---
model: opus
effort: high
pr: batch
automerge: false
---

# Grant quotas are a per-session snapshot, so parallel sessions multiply the budget

## Problem

Found by the security scan of 2026-11-16 (finding M5). Severity: Medium.
Any authenticated user holding a grant with `max_query_counts` or
`max_bytes_transferred` can trigger it, on all five protocols.

The definition's quotas are meant to be one budget per grant. CLAUDE.md:
"One `max_query_counts` and one `max_bytes_transferred` budget is consumed
across the *whole group*". The byte quota is the cap on how much data a grant
can take out. But each session reads the grant's consumption **once, at
connect**, and then only counts its own traffic:

- `populateGrantCounters` (`internal/store/grants.go:495-525`) sums the
  grant's past queries and bytes from the store. It runs from
  `GetActiveGrant`, i.e. at authentication only.
- The per-session checks compare that snapshot plus the session's own
  increments with the limit: PostgreSQL `checkQuotas`
  (`internal/proxy/postgresql/auth.go:243-263`), and
  `internal/proxy/mysql/auth.go:284`, `internal/proxy/mssql/auth.go:65`,
  `internal/proxy/mongodb/auth.go:319`,
  `internal/proxy/oracle/intercept.go:1557`. The counts are only incremented
  in memory (`s.grant.QueryCount++` in each proxy).
- The byte watchdog does the same: `LimitGuard.baseBytes` is set from the
  snapshot (`internal/proxy/shared/limits.go:168-180`, `:237`) and compared at
  `:392`.

So a grant with 1 GB left, opened from N parallel connections, lets each
connection move up to its own 1 GB before any of them notices. That is N
times the budget. The same goes for the query count. Nothing re-reads the
total during a session, and concurrent sessions of a grant (possibly on
different replicas) never see each other's consumption.

## Proposal

1. Add a per-grant consumption reading, refreshed while a session is live:
   - **In process:** a registry keyed by grant UID (next to
     `cache.RevocationRegistry`) with an atomic query counter and an atomic
     byte counter that every session of that grant on this replica adds to.
     The checks and `LimitGuard` read *store baseline + registry live total*,
     not *snapshot + own*.
   - **Across replicas:** re-read the store aggregate on a short tick (the
     limit watchdog's, or the 1-minute byte-notify sweep that already
     computes it, `internal/store/grant_bytes_notify.go`) and replace the
     baseline. Statements count at completion, so the cross-replica view
     lags by up to one tick plus one statement. Document that bound.
2. Avoid double counting: once a statement's row is persisted, the store
   aggregate includes it, so the live in-process counter must only hold what
   is not persisted yet. One approach: the registry keeps "in flight since
   the last baseline", and a refresh subtracts what the new baseline already
   includes.
3. On a trip, the existing `ErrQueryLimitExceeded` / `ErrDataLimitExceeded`
   and `quota_exceeded` termination paths apply unchanged.
4. Document in `docs/` (and in the grant definition form's quota help text)
   that quotas are enforced across sessions, within the refresh bound.

## Tests

- `internal/proxy/shared` (or `internal/cache`) unit test: two sessions on
  one grant with `max_bytes_transferred = B`. Session 1 moves 0.6·B, then
  session 2's check trips once its own 0.6·B passes B in total. Before the
  fix, both reach 0.6·B (1.2·B in total) and neither trips.
- Same for `max_query_counts` with two sessions.
- Store-level test: the periodic baseline refresh picks up a row written by
  "another replica" (inserted directly), and the in-process total is not
  double counted afterwards.
- Positive control: a single session's behaviour is unchanged, and a grant
  with no quotas registers nothing.

## Open questions

- Is a cross-replica lag of about one refresh tick acceptable, or must the
  byte quota be exact across replicas (which needs a store-side reservation
  per statement)? Recommended: the tick. It closes the N-times multiplier,
  and an exact reservation would put a store write on every statement.

## Resolved open questions

- Accept a cross-replica lag of about one refresh tick: do not add a store-side reservation per statement. (autopilot: recommended answer)
