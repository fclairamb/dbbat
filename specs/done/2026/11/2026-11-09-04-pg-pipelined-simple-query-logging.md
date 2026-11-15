---
model: opus
effort: high
pr: batch
automerge: false
---

# PostgreSQL: pipelined simple `Query` messages — all but the last go unlogged

## Problem

Found by the security scan of 2026-11-09 (finding M4). Severity: Medium.
Triggerable by any authenticated dbbat user on a PostgreSQL target whose client
pipelines simple-protocol `Query` messages (libpq pipeline mode / back-to-back
`PQsendQuery` without waiting for `ReadyForQuery`).

The simple-query path holds exactly **one** staged statement,
`s.currentQuery` (`internal/proxy/postgresql/intercept.go:245`). The upstream
reader logs it when its `ReadyForQuery` arrives
(`completeSimpleQuery`, `internal/proxy/postgresql/session.go:1573-1590`), using
the `syncEpoch < upstreamReadyEpoch` guard. But `handleQuery` runs on the
client→upstream goroutine and reads the next client message as soon as it has
forwarded the current one. When a client sends `Query A` then `Query B` in one
flush, `handleQuery(B)` overwrites `s.currentQuery` with B before A's
`ReadyForQuery` has been processed, so A is never logged: no `queries` row, no
error text, no result capture — A is invisible in the UI, the audit chain and
text search, even though it ran upstream.

The extended protocol is unaffected (it stages a *queue*, `pendingQueries`), so
only the simple protocol has the single-slot overwrite. The unlogged statements
also evade the persisted half of `max_query_counts`, which is recomputed as
`COUNT(*)` over `queries` rows at connect (`internal/store/grants.go`), so N−1
pipelined statements per flush do not count against the quota after a reconnect.

(Verified by reading the staging/completion code; not reproduced against a live
pipelining client. The implementer should reproduce first — see Tests.)

## Proposal

1. In `handleQuery` (`internal/proxy/postgresql/intercept.go`), stop clobbering
   a still-pending `s.currentQuery`. Either:
   - stage simple Queries in a FIFO the way the extended path stages
     `pendingQueries`, completing each at its own `ReadyForQuery` (the
     `syncEpoch` bookkeeping already distinguishes them), or
   - if `s.currentQuery` is already non-nil when a new simple Query arrives,
     complete/flush the pending one first (reusing the completion path) before
     staging the new one.
2. Preserve the byte-accounting and `syncEpoch` semantics the current
   `completeSimpleQuery` relies on, and keep the teardown path
   (`completeInFlightOnTeardown`) completing any still-pending entries so the
   hangup accounting from `2026-10-30-05` is not regressed.
3. Charge `max_query_counts` once per actually-forwarded simple statement, not
   once per `currentQuery` slot, so the in-session quota matches the number of
   statements that reached upstream.

## Tests

- `internal/proxy/postgresql/session_bookkeeping_test.go`: drive two
  back-to-back simple `Query` messages through the session before the first
  `ReadyForQuery` is processed; assert **both** produce a `queries` row with the
  correct SQL text (fails before the fix — only the second is logged).
- Integration (`internal/proxy/postgresql/integration_test.go`, behind
  `integration`): pipeline `SELECT 1` then `SELECT 2` in one flush against a
  real PG; both appear in `queries`. Positive control: a single `Query` still
  logs exactly one row (no double-count).
- Quota: N pipelined simple statements in one flush charge N against
  `max_query_counts`, not 1.

## To verify

- Whether the simple-query `syncEpoch` scheme already assigns each pipelined
  Query a distinct epoch (it appears to: `completeSimpleQuery`'s comment says a
  Query staged behind a batch "waits for its own" RFQ at `syncEpoch+1`). If so
  the FIFO approach is a small change; if not, the complete-before-stage
  approach is safer.
