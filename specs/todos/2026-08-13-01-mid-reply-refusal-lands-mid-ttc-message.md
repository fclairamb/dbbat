# A mid-reply refusal lands inside a TTC message and no client can read it

## Goal

Make the ORA-00028 dbbat writes when a byte quota (or expiry, or revocation) is
crossed *while a reply is streaming* readable by the client, instead of being
discarded as a corrupt continuation of the row batch it was injected into.

## Why

Measured against Oracle 23ai Free with a live ojdbc 23.7 and go-ora, in
`TestIntegration_AsyncRefusalAgainstJDBCThin`
(`internal/proxy/oracle/async_refusal_integration_test.go`), through a recording
TCP tap so what dbbat wrote is evidence rather than inference:

- the inline check in `upstreamToClient` fires (not the watchdog), and writes
  **exactly one** well-formed OER: `ORA-00028: session terminated: bandwidth
  quota exceeded for this grant`, carrying a non-zero call number — the number
  of the fetch the client is parked in, exactly as
  `specs/done/2026/08/2026-08-12-02-oracle-async-refusal-call-number.md`
  intended;
- **no client reports it.** ojdbc answers `ORA-03113: database connection closed
  by peer` (`last_rpc=Fetch a row`, cause `ORA-17800: Got minus one from a read
  call`); go-ora answers `driver: bad connection`. Neither mentions ORA-00028.

The call number is *not* the cause — that was the identified risk and it is
ruled out: a rejected call number produces `ORA-18745` (ojdbc's
`handleOutOfSequenceError`, present in this driver's `T4CTTIfun`), and no
ORA-18745 appears. What is wrong is *where* the frame goes. dbbat cuts into the
reply at a **TNS packet** boundary, and a fetch reply is a **TTC message**
stream whose messages straddle packets — that is why `handleContinuation` exists
and carries `lastRow` state across packets. So the OER is delivered in the
middle of a half-transmitted row batch, the client consumes it as row bytes,
keeps waiting for the rest, and meets EOF instead.

The session is torn down either way, and the statement is logged with the real
reason (`aborted: bandwidth quota exceeded for this grant`), so this is a
reporting-quality defect, not an enforcement hole. But it is the difference
between a user being told their quota ran out and a user filing a bug about a
flaky database.

No GitHub issue yet — one should be filed.

## Implementation

The refusal has to wait for a boundary the client will be at, rather than for
the next packet to be forwarded.

- The information is already half there: `interceptUpstreamMessage` decodes each
  upstream message, and `handleQueryResultV2`/`handleContinuation` know when a
  row batch is complete versus continued. What `upstreamToClient` lacks is a
  "the client is between TTC messages right now" predicate. The narrow version:
  set a flag when the decoded upstream message ends cleanly at the end of the
  forwarded packet (the decoder consumed the whole payload), and only run
  `s.guard.Check()`'s refusal branch on such a packet.
- The natural boundary is the reply's own end-of-call OER: hold the violation
  until the in-flight call's OER has been forwarded, then answer the *next*
  client op with the refusal (which is the ordinary answering path,
  `gateStatement`, already measured working on all four clients). That is also
  what a real Oracle does — measured in
  `TestIntegration_RealOracleSessionTermination`: `ALTER SYSTEM KILL SESSION`
  pushes nothing, holds the socket, and answers the session's next call with
  ORA-00028 stamped with that call's number. The cost is one more batch of rows
  streamed past the quota, which is bounded by the fetch size.
- If neither is workable, the honest fallback is to stop writing a frame there
  at all and close the socket, matching `onLimitViolation`: an ORA-03113 that is
  *meant* is better than an ORA-00028 that is written and thrown away, and it
  removes the only place where dbbat writes a frame a client cannot parse.
- Whichever way it goes, the measurement above is the acceptance test: flip the
  two `assert.NotContains(t, output, "midfetch: code=28 ")` /
  `assert.Contains(t, output, "midfetch: code=3113 ")` expectations, and update
  `docs/oracle.md` under "An asynchronous refusal: which call number, and
  whether to send one at all".
