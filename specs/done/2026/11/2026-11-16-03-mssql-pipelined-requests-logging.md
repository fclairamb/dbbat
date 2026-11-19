---
model: opus
effort: high
pr: batch
automerge: false
---

# SQL Server: a pipelined request overwrites the in-flight one, so a statement goes unlogged

## Problem

Found by the security scan of 2026-11-16 (finding M3). Severity: Medium.
Any authenticated user on a SQL Server target can trigger it, under any
grant. It hides statements from the audit trail; it does not bypass a grant
control, because each request is still validated.

The SQL Server proxy assumes TDS without MARS is strictly request/response,
and keeps **one** in-flight statement:

- `pumpClientToUpstream` (`internal/proxy/mssql/relay.go:218-229`) forwards
  each client message as soon as it is intercepted. It does not wait for the
  previous response. The reader may run up to `clientReadAhead` (4) messages
  ahead (`relay.go:44-52`). The comment there assumes the queue only fills
  during an approval hold.
- Each forwarded statement calls `setPending`
  (`internal/proxy/mssql/intercept.go:324`), which **overwrites** the single
  `session.pending` slot (`internal/proxy/mssql/result.go:603-607`).
- At the response's EOM, `finish` → `takePending` (`result.go:610`)
  attributes the response to whatever is pending *then*. A statement's
  `queries` row is written only at that point (see `2026-10-30` M6).

A client that sends request A, then request B, without waiting for A's
response, gets this: A's response is recorded under B's text, parameters,
duration and row capture, and B's response finds no pending statement and is
not recorded. If A is the write, the history contains one row showing B's
text, and none showing A. The query count and the byte accounting are off by
the same amount.

The same pattern was fixed for PostgreSQL simple queries (`2026-11-09` M4,
`specs/done/2026/11/2026-11-09-04-pg-pipelined-simple-query-logging.md`).
Verified in the code; not reproduced against a live SQL Server. Whether the
server executes a request sent before the previous response is complete is
listed under To verify, but the proxy should not depend on it.

## Proposal

Pick one, recommended first:

1. **Serialize at the proxy.** `forwardClientMessage` does not forward a new
   SQLBatch/RPC while a statement is pending. It waits for the response EOM
   that clears `pending`, and only then forwards the next message. ATTENTION
   must still go through at once (it is the cancel path). This makes the
   single slot correct by construction, and the read-ahead queue already
   exists to absorb the wait.
2. **Queue.** Replace the single slot with a FIFO of pending statements, pop
   one per response EOM, and stamp each with the request it answers (the
   PostgreSQL extended-protocol design).

Either way:
- Charge the quota and start the statement clock per request.
- Make sure the approval hold path still works (a held statement is pending
  without having been forwarded).
- Update the comment at `relay.go:44-52`.
- Check the Oracle tracker the same way: `oracleQueryTracker.pendingQuery`
  (`internal/proxy/oracle/intercept.go:200`, `:413`) is a single slot too, and
  `flushPendingQuery` completes the previous query when the next one arrives.
  If the same loss is reachable there, fix it in the same change or file a
  follow-up spec.

## Tests

- `internal/proxy/mssql` unit test with a fake upstream: send two SQLBatch
  messages back to back before any response. Expect two `queries` rows, each
  with its own text, in order, each answered by its own response. Fails
  before the fix (one row, with the wrong text).
- Same for an RPC followed by a SQLBatch.
- ATTENTION sent while a statement is pending still reaches the upstream at
  once.
- Positive control: strictly alternating requests are recorded exactly as
  today.
- Integration (`make test-integration-mssql`), if the server is shown to
  execute pipelined requests: the same two-statement case end to end.

## To verify

- Does SQL Server (2019/2022) execute a second request sent before the first
  response is complete, or reset the connection? The fix is needed either
  way: with option 1 the proxy never forwards it early.
