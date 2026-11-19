---
model: sonnet
effort: medium
pr: batch
automerge: false
---

# A NUL in a statement's error text drops the statement from the query history

## Problem

Found by the security scan of 2026-11-16 (finding M2). Severity: Medium.
Any authenticated user on a SQL Server target can trigger it, under any
grant. MySQL is plausible too (see To verify).

`e81b1a0` routed `sql_text`, `parameters` and `row_data` through the
`storable*` helpers (`internal/store/queries.go:43-158`), because PostgreSQL
refuses a `text`/`jsonb` value containing U+0000. A refused insert drops a
statement that has already run from the `queries` table and from the audit
chain. That commit listed the `error` column as a follow-up, and it is still
stored raw:

- `CreateQuery` copies `query.Error` verbatim (`internal/store/queries.go:165`).
- `UpdateQueryCompletion` sets `error = ?` verbatim
  (`internal/store/queries.go:798`).

On SQL Server the error text is whatever the upstream's ERROR token says, and
T-SQL lets the client choose that text (`THROW` / `RAISERROR` take any
`nvarchar` message, and a message can contain `NCHAR(0)`). A result with no
rows writes its `queries` row in one `CreateQuery` that carries the error
(`internal/proxy/mssql/result.go:673`, `:710-730`). If that insert fails, the
proxy only logs "MSSQL create query log failed" and the row never exists.

So a user can run a batch that does real work (a write, under a full-write
grant) and ends with a client-chosen error carrying a NUL. The batch runs
upstream, and dbbat records nothing: no `queries` row, no chain entry, and no
count against `max_query_counts`. This lets a user remove any batch they
choose from the history. Verified in the code; not reproduced against a live
SQL Server.

## Proposal

1. Add a `storableError(*string) *string` next to `storableSQLText` in
   `internal/store/queries.go`. It applies the same repair:
   `strings.ToValidUTF8` plus NUL → U+FFFD.
2. Use it in `CreateQuery` (`:165`) and in `UpdateQueryCompletion` (`:798`).
3. Check whether the MAC covers `error` (`internal/store/chain*.go`). If it
   does, make sure the MAC is computed over the repaired value, the one that
   is stored, as is already done for `sql_text`.
4. Grep for any other path that writes `queries.error` or
   `connections.termination_*` text from upstream-controlled input, and repair
   it the same way.

## Tests

- `internal/store/queries_test.go`: `CreateQuery` with an `Error` containing
  `\x00` and an invalid UTF-8 byte succeeds. The stored error has U+FFFD in
  their place. Fails before the fix (insert error).
- Same for `UpdateQueryCompletion` with such an error.
- Chain: `dbbat audit verify --queries` (or `VerifyQueryChains`) stays clean
  for a session holding such a row.
- Positive control: an ordinary error string is stored byte for byte.

## To verify

- Whether MySQL's `SIGNAL … SET MESSAGE_TEXT` can carry a 0x00 byte through
  go-mysql into the stored error. If it can, add a MySQL proxy-level case.
