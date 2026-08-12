# Measure the mid-reply ORA-00028 against a live JDBC client

## Goal

Confirm — or refute — with a live ojdbc client that the mid-reply refusal
(`upstreamToClient`'s `writeTTCError(ORA00028, …)`) is reported as itself and not
wrapped in an `ORA-18745`, and that the watchdog's socket close on an idle
client is what a driver renders as a plain I/O error.

## Why

`specs/done/…/2026-08-12-02-oracle-async-refusal-call-number.md` settled *which*
call number an asynchronous refusal carries, and the answer needed no code
change: dbbat never writes a synthesized OER outside a call, so the last number
`observeClientCallNumber` saw is correct at every site that reads it. The
enumeration and the `hasPendingQuery()` gate are pinned by
`TestMidStreamRefusalEndsTheCallTheClientIsWaitingOn` and
`TestIdleLimitViolationSendsNoOER`, and the decision is written up in
`docs/oracle.md` under "An asynchronous refusal: which call number, and whether
to send one at all".

What is **reasoned rather than measured** is the consequence for a live client.
The ojdbc 26.1 sequence-number behaviour was measured for a refusal *answering* a
statement (`TestIntegration_BlockedStatementRefusesJDBCThin`); the argument that
it carries over to a refusal cut into a reply is a protocol argument, not a
capture. It was not measured when the decision was taken because no `ojdbc*.jar`
was reachable on the machine and `ORACLE_TEST_OJDBC_JAR` was unset.

There is one identified way the reasoning could fail, which the measurement would
expose: `hasPendingQuery()` is dbbat's own bookkeeping, not the client's. A
missed end-of-call would leave it set past the real end of a call, and a
violation would then stamp a call the client has already been answered for —
degrading to exactly the pre-fix ORA-18745 wrapping. That is no worse than the
behaviour before the call number existed, and the session tears down regardless,
so it is a reporting-quality risk rather than a correctness one.

## Implementation

- Point `ORACLE_TEST_OJDBC_JAR` at an `ojdbc11.jar` and run the JDBC integration
  path against Oracle 23ai Free, as
  `TestIntegration_BlockedStatementRefusesJDBCThin` already does:
  ```bash
  ORACLE_TEST_OJDBC_JAR=/path/to/ojdbc11.jar \
    go test -tags integration -timeout 40m -run JDBCThin ./internal/proxy/oracle/
  ```
- Add two cases beside it, both on ojdbc **26.1** (23.2 never checks the number,
  so it proves nothing):
  1. a `max_bytes_transferred` quota tripped **mid-result-set** — the client
     must report `ORA-00028`, not `ORA-18745` with ORA-00028 demoted to a cause;
  2. a grant **revoked while the client sits idle** between calls, then a
     statement — the client must see a clean connection-closed error
     (`ORA-17002`/`ORA-03113`), and dbbat must have written no frame.
- Capture what a real Oracle sends for the same situations, for the record:
  `ALTER SYSTEM KILL SESSION` (marks the session; the next call is answered
  `ORA-00028`) versus `... IMMEDIATE` / `DISCONNECT SESSION` (drops the socket).
  The socket tap used for the OER work is the cheapest way to get both.
- If case 1 comes back as `ORA-18745`, the fix is the one the original spec
  guessed at: distinguish *answering this call* from *interrupting the session*
  by tracking in-call state off the client's own ops rather than off
  `hasPendingQuery()`.
- Whatever the outcome, replace the "reasoned, not measured" confidence note in
  `docs/oracle.md` with the measurement.

No GitHub issue yet — file one when picking this up.
