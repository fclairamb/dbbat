---
model: sonnet
effort: low
pr: batch
automerge: false
---

# PostgreSQL `block_ddl`: `EXPLAIN ANALYZE CREATE TABLE … AS` is not recognised as DDL

## Problem

Found by the security scan of 2026-11-16 (finding L1). Severity: Low.
Any authenticated user with a `block_ddl` grant on a PostgreSQL target that is
**not** `read_only` can trigger it. Under `read_only` the server-side pin
already refuses the write.

`EXPLAIN ANALYZE` executes the statement it explains. PostgreSQL accepts
`CREATE TABLE … AS` and `CREATE MATERIALIZED VIEW … AS` as EXPLAIN targets,
so `EXPLAIN ANALYZE` creates the table or view. The `block_ddl` check is
prefix-shaped: `anyStatement(sqlText, isDDLQuery)`
(`internal/proxy/postgresql/intercept.go:151-153`) → `shared.IsDDLQuery` →
`isDDLQuery` (`internal/proxy/shared/validation.go:250-275`), which only reads
the first keyword. `EXPLAIN` is not in `ddlKeywords` (`:143-146`). Checked
against `main`: `IsDDLQuery` returns true for `CREATE TABLE t AS SELECT 1` and
false for the same statement behind `EXPLAIN ANALYZE`.

The impact is limited to creating tables and materialized views: EXPLAIN
takes no `DROP`/`ALTER`. A `block_ddl` grant still promises no schema
changes.

Other dialects: MySQL's `EXPLAIN ANALYZE` takes no DDL, Oracle's `EXPLAIN
PLAN` does not execute, and SQL Server has no EXPLAIN. PostgreSQL only.

## Proposal

1. In `IsDDLQuery` (`internal/proxy/shared/validation.go:250`), when the
   statement starts with `EXPLAIN`, skip the EXPLAIN option list: the
   parenthesised `( … )` form and the bare `ANALYZE` / `VERBOSE` keywords.
   Then classify what remains with the same rules, `selectsIntoTable`
   included. An EXPLAIN that does not execute (no `ANALYZE`) creates
   nothing, but refusing it too is simpler and fail-closed. Decide in the
   open question.
2. Do the same for `isCopyQuery` if EXPLAIN can wrap COPY (it cannot today;
   assert it in a test so a future PostgreSQL change is noticed).

## Tests

- `internal/proxy/shared/validation_test.go`: `IsDDLQuery` is true for
  `EXPLAIN ANALYZE CREATE TABLE t AS SELECT 1`,
  `EXPLAIN (ANALYZE, BUFFERS) CREATE MATERIALIZED VIEW v AS SELECT 1`,
  and `EXPLAIN ANALYZE SELECT 1 INTO t2`. They are false before the fix.
- `internal/proxy/postgresql/intercept_test.go`: under a `block_ddl` grant,
  the first one, alone and as the second statement of a simple-query batch,
  is refused with `ErrDDLNotPermitted`.
- Positive controls: `EXPLAIN ANALYZE SELECT * FROM t` and `EXPLAIN SELECT 1`
  stay allowed under `block_ddl`.

## Open questions

- Should plain `EXPLAIN CREATE TABLE … AS` (no ANALYZE, nothing executed) be
  refused too? Recommended: no. Refuse only when ANALYZE is present, so a
  user can still look at a plan.

## Resolved open questions

- Do not refuse plain `EXPLAIN CREATE TABLE … AS` without ANALYZE: refuse only when ANALYZE is present. (autopilot: recommended answer)
