# Persist blocked statements to query history on PostgreSQL and Oracle

## Goal

When a statement is refused by `read_only`, `block_copy` or `block_ddl`, write a
`queries` row with the `error` set — on **all five** protocols. Today only
MySQL/MariaDB, MongoDB and SQL Server do it.

## Why

A refused statement is the single most interesting thing an access-control proxy
can record: it is an attempt to do something the grant did not permit. On
PostgreSQL and Oracle it currently leaves no row in `queries` and no `audit_log`
entry — it exists only in the process log (`slog` WARN) and in the pcapng
capture if capture happens to be enabled.

That is a real evidence gap. `website/docs/compliance.md` had to call it out
explicitly under "What DBBat does not do", against PCI DSS 10.2.1.4 (audit logs
capture all invalid logical access attempts). Closing it removes a caveat from
the compliance page and makes the UI's query list honest about what was
attempted, not just about what ran.

It is also an inconsistency: three protocols behave one way, two the other, for
no design reason — the PostgreSQL and Oracle paths simply return the error to
the client before `currentQuery` is ever set.

No GitHub issue yet — file one when picking this up.

## Implementation

The three protocols that already do this are the model:

- `internal/proxy/mysql/intercept.go` — `recordQuery(..., &errStr)` on the
  rejection path
- `internal/proxy/mongodb/intercept.go` — `rejectCommand` → `recordQuery`
- `internal/proxy/mssql/intercept.go` — `refuse` → `recordQuery`

The two that need fixing:

- **PostgreSQL**: `internal/proxy/postgresql/session.go` — the simple-query and
  Parse rejection paths call `sendQueryError` and `continue` the loop without
  touching `currentQuery`. Validation itself lives in
  `internal/proxy/postgresql/intercept.go` (bypass-attempt and COPY checks on
  top of `shared.ValidateQuery`). Record the statement with the refusal reason
  as `error` before sending the error to the client. Watch the extended-query
  protocol: a Parse rejection must not double-record when the client then sends
  Bind/Execute, and the client will follow with a Sync.
- **Oracle**: `internal/proxy/oracle/intercept.go` (the "query blocked by access
  control" WARN) and `gateStatement` in `internal/proxy/oracle/session.go`.
  Record before the TTC error is written back.

Details to get right:

- Set `duration_ms` to 0 (or the validation time) and `rows_affected` to 0 — the
  statement never reached the upstream.
- Use the same error-string shape the other three protocols use, so the UI badge
  and any log-based alerting treat all five identically.
- The row must still be attributed to the connection, so it joins to the user and
  database like any other query.

## Tests

- Integration: `make test-integration-postgresql` and `make test-e2e-oracle` —
  assert a `queries` row exists with a non-null `error` after a `read_only` grant
  refuses a write and after a `block_ddl` grant refuses a `CREATE TABLE`.
- Cover both the simple-query and extended-query (Parse/Bind/Execute) paths on
  PostgreSQL.

## Follow-through

Once shipped, remove the "Blocked statements are not uniformly persisted" bullet
from `website/docs/compliance.md` (section "What DBBat does not do") and the
matching caveat in the PCI DSS table.
