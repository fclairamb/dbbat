# SQL Server: `EXEC <procedure>` in a SQLBatch passes read_only

## Goal

A stored procedure (or system procedure such as `sp_cursor`) invoked by name
from a SQLBatch fails closed under a restrictive grant, exactly as the same call
sent as an RPC does.

## Why

Found while implementing `2026-10-30-02-mssql-sp-cursor-positioned-writes`.
On the RPC path, a procedure called by name is refused under `read_only`,
`block_ddl` or `block_copy` (`ErrOpaqueProcedureBlocked`), and `sp_cursor` with
a write optype is now refused too (`ErrCursorWriteBlocked`). On the SQLBatch
path, `interceptSQLBatch` hands the whole text to `validate`, and neither
`EXEC dbo.Recalc` nor `EXEC sp_cursor 7, 1, 1, N'', N'x'` is refused under a
`read_only` grant (checked with a probe on `session.validate`). That is the same
bypass, one message type over: open a cursor with `sp_cursoropen` (its SELECT
passes), then send the positioned UPDATE as batch text.

`docs/mssql.md` (dynamic SQL table) says `EXEC dbo.some_proc` is "already failed
closed under a restrictive grant on the RPC path", which leaves the batch path
unstated.

## Implementation

- In the batch-text scanner (`internal/proxy/shared/dynamicsql.go`) or in
  `interceptSQLBatch`, recognise `EXEC`/`EXECUTE` followed by a procedure name
  (not `(` nor a literal, which are dynamic SQL and already handled) at a
  statement start, and refuse under `grantRestricts` with
  `ErrOpaqueProcedureBlocked`.
- Mind the false positives: `EXEC` inside a string literal or comment, and the
  SQL-carrying system procedures already resolved positionally
  (`sp_executesql`, `sp_prepare`, …), which must keep being checked on their
  statement rather than refused.
- Decide whether a cursor `sp_cursor` call written in a batch gets the optype
  treatment or is simply refused as opaque (simpler, fail closed).
- Tests in `internal/proxy/mssql/intercept_test.go` (`TestSQLBatchEnforcement`):
  `EXEC dbo.Recalc` and `EXEC sp_cursor …` refused under `read_only`, allowed
  under full write; `EXEC sp_executesql N'SELECT 1'` still allowed under
  `read_only`.
- Update `docs/mssql.md`.
