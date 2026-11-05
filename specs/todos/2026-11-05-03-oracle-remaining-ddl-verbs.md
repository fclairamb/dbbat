# Oracle: AUDIT, NOAUDIT, ANALYZE, ASSOCIATE/DISASSOCIATE STATISTICS and LOCK TABLE escape read_only and block_ddl

## Goal

Refuse the remaining Oracle statements that change the data dictionary or take
table locks under `read_only` (and the DDL ones under `block_ddl`), the way
`RENAME`, `PURGE`, `FLASHBACK` and `COMMENT` now are.

## Why

Found while implementing `2026-10-30-03-classify-select-into-and-bulk-insert`.
The shared classifiers are prefix lists (`writeKeywords`, `ddlKeywords` in
`internal/proxy/shared/validation.go`). Oracle documents these as DDL too, and
none of them starts with a listed keyword:

- `AUDIT …` / `NOAUDIT …`: change the audit policy (a dictionary write, and an
  audit trail a read-only user should not be able to switch off).
- `ANALYZE TABLE t COMPUTE STATISTICS` / `ANALYZE … VALIDATE STRUCTURE`:
  rewrites optimizer statistics.
- `ASSOCIATE STATISTICS` / `DISASSOCIATE STATISTICS`.
- `LOCK TABLE t IN EXCLUSIVE MODE`: not a write, but it blocks every writer for
  the length of the transaction; PostgreSQL refuses it in a read-only
  transaction.

Oracle has no session pin, so the text is the control.

## Implementation

- Check that no other dialect starts a read with these words: MySQL has
  `ANALYZE TABLE` (rewrites statistics, a write-ish admin statement) and
  `LOCK TABLES`; PostgreSQL has `ANALYZE` (statistics) and `LOCK`. Decide per
  dialect whether a shared list or an Oracle-only list (in
  `validateOracleStatement`) is the right home; `ANALYZE` on PostgreSQL is run
  by monitoring tools and may need to stay allowed.
- Add to `findSQLKeywords` in `internal/proxy/oracle/ttc_decode.go` whatever the
  gate learns.
- Tests in `internal/proxy/shared/validation_test.go`, with a leading comment,
  and positive controls for columns named `audit_at` / `lock_id`.
