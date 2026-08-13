# Measure the held mid-reply refusal on the other two clients

## Goal

Measure what sqlplus (OCI) and python-oracledb thin do with a byte quota crossed
mid-result-set, now that dbbat holds the refusal and answers the client's *next*
call instead of writing it into the reply in progress
(`session.enforceMidStreamLimits` / `answerHeldRefusal`, added by
`specs/done/.../2026-08-13-01-mid-reply-refusal-lands-mid-ttc-message.md`).

## Why

The fix is measured on two of the four clients dbbat supports:

- **ojdbc 23.7** reports `ORA-00028` where it used to report ORA-03113 — the
  acceptance measurement;
- **go-ora v3** cannot report it and never could: it maps ORA-00028 to
  `driver.ErrBadConn` on purpose (`network.OracleError.Bad()` lists 28 next to
  3113/3114/12537), so its subtest asserts on the tapped frame instead.

sqlplus and python-oracledb are unmeasured on this path, and both have already
been the source of a refusal that dbbat framed *correctly by every other test*
and the client still could not read — that is the entire reason `oerShape`
carries `fixedWidth`, `fixedWidth64` and `endOfResponse`. Two specific risks:

1. the refusal is now written from the **client leg** rather than the response
   leg. Nothing about the frame changed, but the moment it is written did, and
   sqlplus is the client that hangs rather than errors when a frame arrives at a
   moment it does not expect;
2. `answerHeldRefusal` falls back to a bare socket close when the client's next
   call is one dbbat **cannot name** — an unwalkable piggyback. The bundled OCI
   client's own first message is exactly such a frame
   (`gateUnnameableFrame`'s reason for existing), so an OCI session may well take
   the fallback and never see the ORA-00028 at all. Whether it does is a
   measurement, not a guess.

No GitHub issue yet — one should be filed.

## Implementation

- Add the quota-mid-result-set case to the existing per-client integration
  suites rather than a new fixture: `oci_client_integration_test.go` /
  `oci_instantclient_test.go` for sqlplus, and the python-oracledb path used by
  `blocked_integration_test.go`. The fixture work is already there
  (`startOracleThroughProxy`, `testsupport.WithMaxBytesTransferred`, the
  `dbbat_quota_probe` table seeding in `async_refusal_integration_test.go`).
- Put each client behind `startRecordingTap` (`tap_test.go`) so "dbbat wrote a
  readable frame" and "the client reported it" stay separable — that separation
  is what turned the go-ora result from a false negative into a finding.
- Assert the log counters as well as the client output: `logMsgRefusalHeld` then
  `logMsgRefusalDelivered` is the delivered path, `logMsgRefusalUnnameable` is
  the close-instead fallback. If OCI lands on the fallback, that is the result to
  record — and then the follow-up question is whether the held refusal should
  wait for a *nameable* call rather than give up on the first unnameable one.
- Record what is measured in `docs/oracle.md`, "An asynchronous refusal: which
  call number, and whether to send one at all", next to the ojdbc and go-ora
  rows.
