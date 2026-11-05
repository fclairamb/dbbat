---
model: opus
effort: high
---

# PostgreSQL: pipelined Executes store result rows under the wrong statement

## Problem

Found by the security scan of 2026-10-30 (finding M4).

Severity: Medium. Any authenticated user can trigger it, by choice or simply by using a driver that pipelines (a pgx `Batch`, JDBC batches).

With two or more `Execute`s before one `Sync`, the upstream answers `RowDescription(A) DataRow(A)… CommandComplete(A) RowDescription(B) DataRow(B)… CommandComplete(B) ReadyForQuery`. The upstream leg attributes them as follows:

- `getCurrentPendingQuery` (`internal/proxy/postgresql/session.go:926`) returns `currentQuery` if set, else the **last** pending entry. A's `RowDescription` and rows therefore go to B's entry.
- `CommandComplete(A)` calls `popPendingQuery` (`session.go:952`), which promotes A to `currentQuery`. B's rows then go to A's entry.
- `CommandComplete(B)` promotes B to `currentQuery` and overwrites A without completing it. Only B is completed, at `ReadyForQuery` (`session.go:1303-1326`).

Reproduced on 2026-10-30 through the proxy with a two-statement pgx batch: each statement's captured rows were stored under the other's SQL text, and the first statement's row stayed without duration or completion. A user can put the statement whose rows they want to hide first and a decoy second: the query history then shows the sensitive rows under the decoy's SQL. The captured-row chain (`query_rows`) seals rows under the wrong query, so `dbbat audit verify` cannot notice.

## Proposal

1. Make the entry a terminator belongs to the entry that receives that statement's rows. The upstream answers Executes strictly in order, so the head of `pendingQueries` is always the statement being answered. Capture `RowDescription`/`DataRow` into the head, not the tail (`getCurrentPendingQuery`).
2. On each Execute terminator (`CommandComplete`, `ErrorResponse`, `EmptyQueryResponse`, `PortalSuspended`), complete and log the head entry right away, with its own rows-affected, error and duration, then drop it. Do not park it in `currentQuery` until `ReadyForQuery`. Keep the simple-query path (`currentQuery`) as is.
3. Revisit byte attribution: today it is a diff between `ReadyForQuery`s. Attribute per terminator, or keep per-batch attribution on the last entry, and document which.
4. Keep `reconcilePendingQueries` (`session.go:980` onwards) as the backstop, and its "no WARN in normal flows" unit assertions.

## Tests

- Integration (`internal/proxy/postgresql/integration_test.go`): a pgx `Batch` of two SELECTs returning distinct values. Each `queries` row has its own SQL, its own captured rows, a duration and no error. This fails before the fix (rows swapped, first row incomplete).
- Same with three statements, the middle one failing: the rows after the error are discarded by the server, and the entries are completed by the reconcile with the existing message.
- Positive control: simple-query multi-statement and single-Execute flows keep their current rows and counters (the existing capture tests).
- Unit tests next to the existing `popPendingQuery` / reconcile tests for `PortalSuspended` paging: N pages still give N rows, each with its own duration.
