# An asynchronous Oracle refusal ends a call the client is not waiting on

## Goal

Decide what call number a refusal that is *not* answering a client request
should carry, and either carry it or say why zero (or the last one) is right.

## Why

`oerSummary.CallNumber` now stamps the TTC sequence number of the client's own
request onto every synthesized OER, because ojdbc 26.1 will not read the error
out of a frame whose call number is not the one it sent — it reports
`ORA-18745: Execution error in sessionless transaction piggybacked call` with
the real ORA-01031 demoted to a cause (see `docs/oracle.md`, "Ending a call: the
OER encoder", and `fix(oracle): stamp the client's call number on a synthesized
OER`).

The number is recorded by `session.observeClientCallNumber`, off every client
message that reaches `interceptClientMessage`. That is exactly right for a
refusal answering a statement, which is the overwhelmingly common case and the
one measured live against Oracle 23ai Free.

It is *not* obviously right for the refusals that are not answers:

- the limit watchdog's `writeTTCError(ORA00028, …)` in `session.go`, which fires
  on revocation or expiry while the client may be idle between calls;
- a limit violation caught on the **response** leg, where the client's call has
  already been forwarded upstream and dbbat is cutting into a reply.

In both cases dbbat stamps the last op the client sent, which is a call it is no
longer waiting on. Nothing has been measured there: it may be exactly what a
server does when it kills a session, or ojdbc may take it as out of sequence and
mislabel the error the same way it did before this fix.

## Implementation

- Reproduce with a live JDBC client (`ORACLE_TEST_OJDBC_JAR`, see
  `TestIntegration_BlockedStatementRefusesJDBCThin`): a grant revoked
  mid-session while the client sits idle, then a statement — and separately, a
  `max_bytes_transferred` quota tripped mid-result-set.
- Compare against what a real Oracle sends when it terminates a session
  (`ALTER SYSTEM KILL SESSION` produces an ORA-00028 the same clients parse);
  the tap in the OER work is the cheapest way to capture one.
- If the number matters there, the fix is probably to distinguish "answering
  this call" from "interrupting the session" rather than to always reuse the
  last observed op.

No GitHub issue yet — file one when picking this up.
