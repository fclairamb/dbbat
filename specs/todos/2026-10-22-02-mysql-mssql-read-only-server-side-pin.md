# MySQL and SQL Server read_only: pin the session read-only on the server

## Goal

Give `read_only` on MySQL/MariaDB and SQL Server a server-side backstop, the way
PostgreSQL has `default_transaction_read_only`, so that a write the text checks
miss is refused by the database.

## Why

On both protocols `read_only` is the text classification alone
(`shared.ValidateMySQLQuery`, `shared.ValidateQuery`). The 2026-10-22 fix closed
the writing-CTE hole for them (`WITH c AS (…) DELETE …`), but a stored
procedure call, a function with side effects, or any spelling the classifier
does not know still writes. PostgreSQL is now protected by the pin plus a
tripwire; these two have neither.

## Implementation

- MySQL/MariaDB: issue `SET SESSION TRANSACTION READ ONLY` in the session setup
  that already sets `max_execution_time` / `max_statement_time`, fail the
  session if it cannot be set, and refuse `SET [SESSION] TRANSACTION READ WRITE`,
  `START TRANSACTION READ WRITE`, `SET [SESSION] transaction_read_only`/
  `tx_read_only` and `SET @@…transaction_read_only` under `read_only` — the same
  shape as `readOnlyBypassPatterns` in `internal/proxy/postgresql/intercept.go`.
  Check whether MySQL's `SESSION_TRACK_SYSTEM_VARIABLES` can report the variable
  for a tripwire equivalent.
- SQL Server has no session read-only mode. Options: `ApplicationIntent=ReadOnly`
  only routes to a readable secondary when an availability group exists; short of
  that, document `read_only` on SQL Server as text-only and recommend a login
  with `db_datareader` only.
- Refuse `CALL` (MySQL) and `EXEC <proc>` (SQL Server) under `read_only`, as
  Oracle and PostgreSQL now do (`shared.ErrReadOnlyProcedureCall`), once the
  impact on clients that call procedures at connect time has been measured.

Key files: `internal/proxy/mysql/`, `internal/proxy/mssql/`, `docs/mysql.md`,
`docs/mssql.md`.
