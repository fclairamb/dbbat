---
model: opus
effort: medium
---

# SQL Server: a statement whose client disconnects before the response is never logged

## Problem

Found by the security scan of 2026-10-30 (finding M6).

Severity: Medium. Triggered by any authenticated user on a SQL Server server.

The SQL Server proxy creates the `queries` row only when the response completes: `recordQuery`/`persistQuery` run from `finish` once the response's end-of-message packet is relayed (`internal/proxy/mssql/result.go:662`, `CreateQuery` at `result.go:699`). At teardown, `recordDisconnect` (`internal/proxy/mssql/session.go:674`) completes the in-flight `pending` statement only when a termination reason is set (`session.go:692-700`).

A client that sends a statement and closes its socket right away gets the statement forwarded and executed upstream. The proxy then discards `pending`, so no `queries` row is written, and the statement is missing from the query history, the UI and the per-connection query chain. Only the `.pcapng` capture, when enabled, still has it. This breaks the "every query logged" guarantee for statements that change data.

## Proposal

1. In `recordDisconnect`, always complete an outstanding `pending` (`s.takePending()`): with the termination message when there is one, otherwise with an error such as "client disconnected before the response completed".
2. Check the MongoDB proxy for the same shape (row created at completion, pending dropped on a plain disconnect) and fix it the same way if needed. MySQL runs the upstream exchange synchronously in the command handler, so it is likely unaffected.

## Tests

- `internal/proxy/mssql` unit test with the existing fake upstream: send a SQLBatch, close the client before the upstream answers, run teardown. A `queries` row exists with the disconnect error. This fails before the fix.
- Positive control: a statement that completes normally is recorded once, with no duplicate row at teardown.

## To verify

- Whether the MongoDB proxy (`internal/proxy/mongodb/result.go`, `session.go:741`) records a statement whose client disconnects before the reply.
