# PostgreSQL: set_config can widen the pinned statement_timeout

## Goal

Refuse `set_config('statement_timeout', …)` when a statement limit is pinned,
like `SET statement_timeout` already is.

## Why

`statementTimeoutBypassPatterns` in `internal/proxy/postgresql/intercept.go`
refuses `SET/RESET statement_timeout` and `RESET ALL`, but
`SELECT set_config('statement_timeout', '0', false)` is the same SET as a
function. The watchdog still enforces the ceiling, so the effect is that the
client meets a dropped socket instead of SQLSTATE 57014 — a courtesy lost, not a
limit lifted. Found while closing the read-only set_config route (2026-10-22).

## Implementation

In `validateStatement`, when `s.statementLimit > 0`, also refuse
`shared.PostgresSetConfigReaches(sqlText, "statement_timeout")` with
`ErrStatementTimeoutManaged`. Add the case to the statement-timeout unit and
integration tests.
