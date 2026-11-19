---
model: opus
effort: medium
pr: batch
automerge: false
---

# Oracle `ALTER USER … IDENTIFIED BY` and SQL Server `ALTER LOGIN … PASSWORD` evade the password-change block

## Problem

Found by the security scan of 2026-11-16 (finding M4). Severity: Medium.
Any authenticated dbbat user with a **full-write** grant on an Oracle or SQL
Server target can trigger it. `read_only` and `block_ddl` already refuse it
as a write/DDL.

dbbat refuses password changes under every grant, because all dbbat sessions
to a server log in as **one shared upstream account**. A proxied user who
changes that account's password breaks dbbat's stored credential, and every
other user's next connection to the server fails. `2026-11-09-01` closed this
for MySQL (`mysqlPasswordChange`, `internal/proxy/shared/validation.go:860`),
and the `e81b1a0` commit message lists Oracle as the remaining gap.

The shared classifier `isPasswordChangeQuery`
(`internal/proxy/shared/validation.go:593-602`) only fires on
`ALTER USER`/`ALTER ROLE` followed by the word `PASSWORD`. It is called from
`validateQuery` (`:617-631`), which the Oracle and SQL Server paths use.
Checked against `main` with a validator-level test, under a full-write grant:

- Oracle `ALTER USER <u> IDENTIFIED BY <pw>` (the standard Oracle spelling; a
  user can change their own password without the old one under the default
  profile) is **allowed**. `ALTER USER <u> PASSWORD …` is refused.
- SQL Server `ALTER LOGIN <l> WITH PASSWORD = …` is **allowed**: the prefix
  is `ALTER LOGIN`, which the classifier does not know. Changing one's own
  password needs `OLD_PASSWORD`, which the user does not have. Changing
  another login's needs `ALTER ANY LOGIN`, which a shared admin-ish upstream
  account often has.

## Proposal

1. Oracle: in the Oracle path (`ValidateOracleQuery` /
   `validateOracleStatement`, `internal/proxy/shared/validation.go:703`,
   `:781`), refuse `ALTER USER … IDENTIFIED …` and `CREATE USER … IDENTIFIED …`
   with `ErrPasswordChangeBlocked` under every grant, on the normalised
   scratch copy. `IDENTIFIED EXTERNALLY` / `GLOBALLY` changes how the user
   authenticates and should be refused too.
2. SQL Server: in `ValidateMSSQLBatch` (`internal/proxy/shared/mssql_batch.go:31`),
   refuse any statement of the batch, and of every unwrapped dynamic
   payload, that is `ALTER LOGIN … WITH … PASSWORD` or
   `CREATE LOGIN … WITH PASSWORD`. Also refuse `ALTER USER … WITH PASSWORD`,
   the contained-database spelling.
3. Keep these dialect-scoped (as `mysqlPasswordChange` is), so PostgreSQL's
   behaviour does not change.
4. Update the protocol docs (`docs/oracle.md`, `docs/mssql.md`) where they
   list what is always refused.

## Tests

- `internal/proxy/shared/validation_test.go`, Oracle, full-write grant:
  `ALTER USER app IDENTIFIED BY x`, `CREATE USER u2 IDENTIFIED BY x`, and the
  comment-split spelling are refused with `ErrPasswordChangeBlocked`. They
  pass before the fix.
- SQL Server (`ValidateMSSQLBatch`), full-write grant: `ALTER LOGIN l WITH
  PASSWORD = 'x'`, the same as the second statement of a batch, and inside
  an `sp_executesql` payload, are all refused.
- Positive controls that stay allowed under full write: Oracle `ALTER TABLE
  t ADD (identified NUMBER)`, `SELECT identified FROM t`. SQL Server `ALTER
  LOGIN l DISABLE` is refused only by the grant controls, as today. Pick and
  pin the expected value.
- Unchanged: Oracle `ALTER USER u PASSWORD EXPIRE` stays refused.
