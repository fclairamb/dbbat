---
model: opus
effort: medium
---

# SELECT ... INTO, BULK INSERT and Oracle RENAME/PURGE/FLASHBACK escape the write and DDL classifiers

## Problem

Found by the security scan of 2026-10-30 (finding M3; the Oracle verbs were reported by the Oracle pass of the same scan).

Severity: Medium. Triggered by an authenticated user with a `read_only` grant on SQL Server or Oracle, or a `block_ddl` grant on SQL Server, Oracle or PostgreSQL.

The shared classifiers are prefix lists: `writeKeywords` (`internal/proxy/shared/validation.go:115-118`) and `ddlKeywords` (`validation.go:121`). Two statements that write, or create a table, start with neither:

- `SELECT <columns> INTO <new table> FROM ...` creates a table and fills it. It starts with `SELECT` and is not a writing CTE, so `read_only` and `block_ddl` both let it through. On PostgreSQL the `read_only` pin still refuses it, but `block_ddl` (a text check only) does not. On SQL Server, where `read_only` is the text control alone, it passes both.
- On Oracle, `RENAME <table> TO <name>` (a schema change), `PURGE TABLE` / `PURGE RECYCLEBIN` (permanent removal of dropped tables), `FLASHBACK TABLE … TO TIMESTAMP|SCN|BEFORE DROP` (rewinds a table's rows or restores a dropped one) and `COMMENT ON` (a data-dictionary write) start with none of the listed keywords. They pass `read_only` and `block_ddl` on all three Oracle statement paths (`validateOracleStatement`, `validation.go:673-691`). Oracle has no session pin, so `docs/oracle.md` ("Static PL/SQL and procedure calls under read_only") calls the text "the control" and "a strong filter on what the client *types*". These are typed statements.
- `BULK INSERT <table> FROM <file>` on SQL Server loads rows into a table. It is matched only by `bulkCopyPattern` under `block_copy` (`internal/proxy/mssql/intercept.go:26`, checked at `intercept.go:389-392` in `checkStatement`). A `read_only` grant without `block_copy` forwards it.

## Proposal

1. Recognise `SELECT ... INTO <identifier>` as both a write and DDL in the shared classifier. Scan the comment-normalized text with literals skipped, at statement level, so that these do not match: Oracle/PL-SQL `SELECT ... INTO v` inside a block (already handled for Oracle, see `validation_test.go:124`), MySQL `INTO OUTFILE/DUMPFILE/@var` (already blocked separately), and `INSERT INTO`. Scope it per dialect if a shared rule cannot tell `SELECT INTO var` from `SELECT INTO table` (MySQL `SELECT ... INTO @v` is a variable assignment, not a table).
2. Add `RENAME`, `PURGE`, `FLASHBACK` and `COMMENT` to the write list, and `RENAME`, `PURGE`, `FLASHBACK` to the DDL list. Check first that no other dialect starts a read with these words. `COMMENT ON` is also valid on PostgreSQL, where the read-only pin refuses it anyway and `block_ddl` should refuse it too.
3. In `internal/proxy/mssql/intercept.go` `checkStatement`, refuse `bulkCopyPattern` matches under `read_only` too, not only under `block_copy`, since `BULK INSERT` and `INSERT BULK` write rows.

## Tests

- `internal/proxy/shared/validation_test.go`: `SELECT a INTO t2 FROM t` is a write and DDL for PostgreSQL and SQL Server text. A comment between `INTO` and the name does not hide it. This fails before the fix.
- Positive controls: `SELECT ... INTO v` inside an Oracle PL/SQL block, MySQL `SELECT ... INTO @v`, `INSERT INTO t SELECT ...` (already a write) and a plain `SELECT` with a column named `into_x` keep their current classification.
- `internal/proxy/shared/validation_test.go`: under Oracle `read_only` and `block_ddl`, `RENAME t TO u`, `PURGE TABLE t`, `FLASHBACK TABLE t TO BEFORE DROP` and `COMMENT ON TABLE t IS 'x'` are refused, including with a leading comment. This fails before the fix. A `SELECT` from a column named `rename_at` and `SELECT … FROM recyclebin` are still allowed.
- `internal/proxy/mssql/intercept_test.go`: `BULK INSERT` is refused under `read_only` without `block_copy`, and still allowed under a full-write grant.

## To verify

- Whether MySQL/MariaDB accept `SELECT ... INTO <table>` in any mode (they do not as standard SQL; `CREATE TABLE ... SELECT` is already caught by `CREATE`).
