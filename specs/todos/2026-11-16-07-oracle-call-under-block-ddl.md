---
model: sonnet
effort: low
pr: batch
automerge: false
---

# Oracle: `CALL` is not refused under `block_ddl`

## Problem

Found by the security scan of 2026-11-16 (finding L2). Severity: Low.
Any authenticated user with a `block_ddl` grant (not `read_only`) on an
Oracle target can trigger it. It needs a stored procedure that runs DDL to
already exist.

What a stored procedure does lives in the database, not in the statement
text, so every other dialect refuses a procedure call under `block_ddl`:
PostgreSQL refuses `DO`/`CALL` (`internal/proxy/postgresql/intercept.go:157-165`),
and MySQL refuses `CALL` since `2026-11-09-02`. On Oracle, `CALL` is refused
only under `read_only`: `validateOracleReadOnlyPLSQL`
(`internal/proxy/shared/readonly_scan.go:58-60`) is called inside `if
grant.IsReadOnly()` in `validateOracleStatement`
(`internal/proxy/shared/validation.go:790-794`). Checked against `main`:
`ValidateOracleQuery("CALL pkg.run()", block_ddl grant)` returns nil.

This is Low, not Medium, because Oracle has a second spelling dbbat already
documents as out of reach: an anonymous block that calls a procedure
(`BEGIN pkg.proc; END;`) is not refused under `read_only` either
(`readonly_scan.go:50-57`; refusing it would refuse the
`BEGIN DBMS_OUTPUT.ENABLE; END;` that every SQL client sends). So refusing
`CALL` makes the dialects consistent and closes the direct form. The block
form stays a documented limitation, for which a DDL-less database account is
the real guarantee.

## Proposal

1. In `validateOracleStatement` (`internal/proxy/shared/validation.go:781`),
   refuse a statement starting with `CALL` under `ShouldBlockDDL()` as well,
   with a block_ddl error (`ErrDDLBlocked`, or a dedicated
   `ErrDDLProcedureCall` mirroring PostgreSQL's). Use the same
   `startsWithKeyword` test `validateOracleReadOnlyPLSQL` uses.
2. Update `docs/oracle.md`: `block_ddl` refuses `CALL`, and the anonymous
   block calling a procedure stays out of reach, as under `read_only`.

## Tests

- `internal/proxy/shared/validation_test.go` (Oracle table): under a
  `block_ddl` grant, `CALL pkg.run()` and `call pkg.run()` (lower case,
  leading comment) are refused. They pass before the fix.
- Positive controls under `block_ddl`: `SELECT 1 FROM dual`,
  `BEGIN DBMS_OUTPUT.ENABLE; END;` stay allowed. Under a full-write grant,
  `CALL pkg.run()` stays allowed.
