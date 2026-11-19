---
model: opus
effort: low
---

# MySQL: always-blocked server-wide statements have unblocked spellings

## Problem

Found by the security scan of 2026-10-30 (finding L10).

Severity: Low. Triggered by an authenticated user, including one with a `read_only` grant. Needs an upstream account with the matching server privilege (`SYSTEM_VARIABLES_ADMIN`/`SUPER`, `FILE`, `SHUTDOWN`).

`mysqlBlockedPatterns` refuses server-wide operations whatever the grant says (`internal/proxy/shared/validation.go:150-165`). It matches `SET GLOBAL` and `LOAD DATA [LOCAL] INFILE`. Other spellings of the same operations get through:

- `SET @@GLOBAL.<var> = …` and `SET @@global.<var>` (the scope as a prefix), `SET PERSIST` and `SET PERSIST_ONLY` (persisted across restarts). These change the server for every session, for example its general log file.
- `LOAD XML [LOCAL] INFILE` reads a server-side file into a table, like `LOAD DATA INFILE`.
- The SQL statements `SHUTDOWN` and `RESTART`. Only the `COM_SHUTDOWN` protocol command is refused (`internal/proxy/mysql/intercept.go:196-205`).

None of these start with a write or DDL keyword, so a `read_only` grant does not stop them, and the read-only pin only covers transactions. The statement-timeout patterns already handle the `@@` and `PERSIST` spellings (`intercept.go:754-761`).

## Proposal

1. Extend `mysqlBlockedPatterns`: the `SET` scope spellings (`GLOBAL`, `PERSIST`, `PERSIST_ONLY`, `@@GLOBAL.`, `@@PERSIST.`, `@@PERSIST_ONLY.`), anywhere in a `SET` list, not only as its first item. Add `LOAD XML … INFILE`, and `SHUTDOWN` / `RESTART` as statements.
2. Keep them comment-normalized like the rest of the list.

## Tests

- `internal/proxy/shared/validation_test.go`: each spelling above is refused by `ValidateMySQLQuery` under a full-write grant, including `SET @@session.x=1, @@global.y=2`. This fails before the fix.
- Positive control: `SET @@session.sql_mode = …`, `SET SESSION x = …`, `SET @user_var = 1` and `SELECT @@global.version` are still allowed.
