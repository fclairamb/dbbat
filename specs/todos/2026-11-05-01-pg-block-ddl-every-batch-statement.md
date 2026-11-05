# PostgreSQL: block_ddl only checks the first statement of a simple-query batch

## Goal

Under `block_ddl`, refuse a simple-query string when **any** of its statements
is DDL, not only the first one: `SELECT 1; DROP TABLE t` must be refused.

## Why

Found while implementing `2026-10-30-01-pg-block-ddl-refuses-do-and-call`.
`validateStatement` (`internal/proxy/postgresql/intercept.go`) calls
`isDDLQuery`, which is `shared.IsDDLQuery`: a `HasPrefix` on the normalized text
(`internal/proxy/shared/validation.go`, `isDDLQuery`). A simple `Query` message
can carry several statements, and PostgreSQL runs them all, so a
`block_ddl` grant (not `read_only`) can still drop a table by putting a harmless
statement first. `read_only` already classifies every statement of a batch
(`validateReadOnlyBatch`), and the MSSQL path has its own batch scan
(`shared/mssql_batch.go`), so PostgreSQL `block_ddl` is the odd one out.

## Implementation

- Reuse the statement splitter `validateReadOnlyBatch` uses (literal-, comment-
  and dollar-quote-aware) and run `isDDLQuery` on each statement under
  `block_ddl`; return `ErrDDLNotPermitted` on the first hit.
- Check whether `block_copy` has the same gap (`isCopyQuery` is also a prefix
  check) and fix it the same way.
- Tests in `internal/proxy/postgresql/intercept_test.go` through `handleQuery`:
  `SELECT 1; DROP TABLE t`, `SELECT ';'; CREATE TABLE t (a int)` refused;
  `SELECT 'x; DROP TABLE t'` allowed. Integration: a batch dropping a table
  leaves it in place.
- Document it in `docs/postgresql.md` ("Block-DDL grants").
