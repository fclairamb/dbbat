---
model: opus
effort: medium
pr: batch
automerge: false
---

# MySQL: `ALTER USER … IDENTIFIED BY` evades the password-change block

## Problem

Found by the security scan of 2026-11-09 (finding M1). Severity: Medium.
Triggerable by any authenticated dbbat user holding a **full-write** grant on a
MySQL/MariaDB target (a `read_only` grant already blocks it as a write).

dbbat refuses statements that change a user's password on every protocol: PG
does it unconditionally (`internal/proxy/postgresql/intercept.go:128` →
`ErrPasswordChangeBlocked`), and the shared `validateQuery` enforces the same
for MySQL (`internal/proxy/shared/validation.go:609`,
`ValidateMySQLQuery` → `validateQuery`). The reason is that every dbbat session
to a given server logs in as **one shared upstream role**: letting a proxied
user change that role's password desyncs dbbat's stored (AES-GCM encrypted)
credential, so every *other* user's future connections to that server fail, and
existing sessions fail on their next reconnect — a denial of service for the
whole database, caused by one user.

The classifier `isPasswordChangeQuery`
(`internal/proxy/shared/validation.go:585`) only fires when the literal word
`PASSWORD` follows `ALTER USER`/`ALTER ROLE`. MySQL's standard spelling carries
no `PASSWORD` token:

- `ALTER USER CURRENT_USER() IDENTIFIED BY 'x'` — changes one's own password,
  needs no special upstream privilege.
- `ALTER USER u IDENTIFIED WITH caching_sha2_password BY 'x'`
- `CREATE USER … IDENTIFIED BY 'x'`, `GRANT … IDENTIFIED BY 'x'` (legacy).

`validation_test.go` even pins `ALTER USER u IDENTIFIED BY 'x'` as "ordinary
DDL". So the block that exists is simply blind to the most common MySQL
password-change form.

## Proposal

1. Extend `isPasswordChangeQuery`
   (`internal/proxy/shared/validation.go:585`) — or add a MySQL-specific
   predicate called from `ValidateMySQLQuery` — to also refuse, on an
   already-normalised scratch copy:
   - `ALTER USER`/`ALTER ROLE` followed anywhere by `IDENTIFIED`
     (covers `IDENTIFIED BY`, `IDENTIFIED WITH … BY`, `IDENTIFIED WITH … AS`).
   - `CREATE USER … IDENTIFIED …` and `GRANT … IDENTIFIED BY …`.
   Keep the existing `ALTER USER … PASSWORD` arm for the Oracle/standard
   spelling.
2. Scope the `IDENTIFIED` arm so it cannot be defeated by a comment
   (`ALTER/**/USER`) — reuse the `matchableSQL` normalisation the function
   already runs on, and match on word boundaries so a column named `identified`
   in a `SELECT` is not caught (the `ALTER USER`/`CREATE USER`/`GRANT` prefix
   gate already prevents that).
3. Confirm PG is unaffected: PG's own `isPasswordChangeQuery`
   (`internal/proxy/postgresql/intercept.go:544`) delegates to the shared one,
   and PG uses `ALTER ROLE … PASSWORD`/`ENCRYPTED PASSWORD`, which the existing
   arm already catches — the new `IDENTIFIED` arm must not change PG behaviour
   (PG has no `IDENTIFIED BY`).

## Tests

- `internal/proxy/shared/validation_test.go` (the MySQL validation table): under
  **any** grant, each of these is refused with `ErrPasswordChangeBlocked`:
  `ALTER USER CURRENT_USER() IDENTIFIED BY 'x'`,
  `ALTER USER u IDENTIFIED WITH caching_sha2_password BY 'x'`,
  `CREATE USER a IDENTIFIED BY 'x'`,
  `ALTER/**/USER u IDENTIFIED BY 'x'` (comment-normalised). These fail before the
  fix (currently classified as ordinary DDL / allowed under full write).
- Positive controls that must stay allowed under a full-write grant:
  `SELECT identified FROM audit` (column named `identified`),
  `ALTER TABLE users ADD COLUMN identified bool`.
- Positive control that must stay refused unchanged:
  `ALTER USER u PASSWORD EXPIRE` (existing `PASSWORD` arm).
- Flip the existing `validation_test.go` assertion that classifies
  `ALTER USER u IDENTIFIED BY 'x'` as ordinary DDL.

## To verify

- The exact call site(s) where `ValidateMySQLQuery` runs the shared
  `validateQuery` for `COM_QUERY` and `COM_STMT_PREPARE`, so the new arm covers
  both the simple and prepared paths.
- Whether MariaDB's `SET PASSWORD FOR u = PASSWORD('x')` is already caught by
  `mysqlBlockedPatterns` (`SET PASSWORD`) — if so, no change needed there.
