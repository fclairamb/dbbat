---
model: opus
effort: low
pr: batch
automerge: false
---

# MySQL: `CALL` is permitted under `block_ddl`

## Problem

Found by the security scan of 2026-11-09 (finding M2). Severity: Medium.
Triggerable by any authenticated dbbat user holding a `block_ddl` grant on a
MySQL/MariaDB target.

Both the PG and MySQL proxies treat a stored-procedure `CALL` as "code dbbat
cannot read" (the body can run any DDL, directly or via dynamic SQL), so a
restrictive grant must refuse it. PG refuses `CALL` under **both** `read_only`
and `block_ddl` (`internal/proxy/postgresql/intercept.go:163-166`,
`ErrDDLProcedureCall`; rationale in
`internal/proxy/postgresql/errors.go:44-50`).

MySQL refuses it only under `read_only`:
`internal/proxy/shared/validation.go:803-813` enforces `mysqlProcedureCall`
inside `if grant.IsReadOnly()` only. A `block_ddl`-only MySQL grant therefore
forwards `CALL rebuild_schema()` / `CALL migrate()`, whose body runs DDL — the
exact statement PG refuses for the same grant. This is the gap noted (but never
specced) as a variant in the 2026-11-02 scan report.

## Proposal

1. In `internal/proxy/shared/validation.go` (the MySQL `ValidateMySQLQuery`
   path around line 803), enforce `mysqlProcedureCall` under
   `grant.ShouldBlockDDL()` as well as `grant.IsReadOnly()`.
2. Return a distinct error for the DDL case (mirror PG: keep
   `ErrReadOnlyProcedureCall` for read-only, add an `ErrDDLProcedureCall`
   equivalent for block_ddl) so the client message names the right control, and
   so the refusal is logged accurately.
3. Keep the existing `(^|;)\s*CALL\b` anchor so a `CALL` after a comment or a
   leading statement is still caught and a column/identifier named `call` is
   not.

## Tests

- `internal/proxy/shared/validation_test.go`: `CALL do_migration()` under a
  `block_ddl`-only grant is refused (fails before the fix — currently allowed);
  under `read_only` still refused; under full-write still allowed.
- Positive control: `SELECT call_count FROM t` under `block_ddl` allowed
  (identifier starting with `call`).
- Positive control: `SELECT 1; CALL p()` under `block_ddl` refused (anchor).

## To verify

- That no MySQL-package test currently asserts `CALL` is allowed under
  `block_ddl` (which would need flipping).
