---
model: sonnet
effort: medium
pr: batch
automerge: false
---

# Oracle: a pipelined call's result and duration are credited to the next statement

## Problem

Found while fixing the SQL Server equivalent (`2026-11-16-03`). Severity: Low.
Any authenticated user can trigger it by sending a TTC call before the
previous call's response.

The Oracle proxy keeps one in-flight statement (`s.tracker.pendingQuery`).
When a new statement starts, `flushPendingQuery`
(`internal/proxy/oracle/intercept.go`) first completes the one in the slot, so
no statement is lost from the history. But when the client pipelines:

- the first statement is recorded with no result (no rows affected, no
  error) and a duration that ends when the second call arrived, not when the
  server answered it;
- the first statement's response (rows, `ORA-` error, end of call) is then
  read by `completeQueryFromOER` against the second statement, which gets the
  first one's row count or error, and a duration that starts too early or
  ends too early.

So an audit row can show a failed `DELETE` as successful and a harmless
`SELECT` as the one that raised the error. Nothing is unlogged, which is why
this is Low.

## Proposal

Make the accounting match responses to calls in order, as the SQL Server
proxy now does, by one of:

1. Serialize: hold a client call that is not a break/reset marker until the
   upstream has ended the previous call, the same as `claimUpstreamTurn` in
   `internal/proxy/mssql/relay.go` (bounded read-ahead, break/attention goes
   through at once so cancel still works). Preferred, if TTC's
   end-of-call is reliably detectable for every call the proxy tracks (OER,
   end of fetch, piggybacks).
2. Queue: replace the single slot with a FIFO of pending statements, each
   completed by the next end-of-call. Riskier, since a miscounted end-of-call
   shifts every later row.

## Tests

- Two calls sent back to back before the first response: each history row has
  its own text, its own row count / error and a duration bounded by its own
  response.
- A break sent while a call runs still reaches the upstream at once.
- A client that hangs up, and an upstream that hangs up, while a call waits
  its turn both end the session (see the mssql `WaitsItsTurn` tests).

## Accepted limits

- A multi-packet response's tail can still be credited to the next pipelined
  call: the held call is released on the first upstream packet of the previous
  answer, because the end of a call is not reliably detectable in the TNS
  stream. Waiting for a reliable end marker would risk stalling or deadlocking
  sessions, so the first-packet release is the accepted trade-off.

## To verify

- Whether any real Oracle driver (JDBC thin, python-oracledb thin, OCI)
  actually pipelines calls outside of piggybacked messages, which the proxy
  must keep treating as part of the following call.

Status: unverified here. This cannot be checked without a real Oracle; it is
left to the nightly integration suite.
