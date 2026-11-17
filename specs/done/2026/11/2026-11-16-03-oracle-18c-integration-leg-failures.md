---
model: opus
effort: high
pr: batch
automerge: false
---

# The Oracle 18c XE integration leg fails 14 tests: refusals kill sqlplus sessions, JDBC thin sees protocol violations, long statements and tags are lost

GitHub Issue: https://github.com/fclairamb/dbbat/issues/505

## Source

Follow-up of #500 / #502. Once the go-ora v3 Accept panic was fixed (fork
`fclairamb/go-ora v3.0.2-fclairamb.1`), the `oracle (18c XE (pinned))` leg of the nightly
`Integration tests` workflow ran past its first test and 14 tests failed (run 43926482614,
job 133507220967, commit 92d40a7). The 23ai legs are green on the same run. The 18c image
(`gvenzl/oracle-xe:18.4.0-slim`) is amd64-only, so reproduce in CI
(`gh workflow run "Integration tests" --ref <branch>`) or on an amd64 host.

The report is right on the list and the symptoms. Its guess ("18c speaks an older TTC/TNS,
version 315") needs care: 18c negotiates v315, the same threshold dbbat already treats as
"modern" in several places (`markers.go:43`, `intercept.go:831`, `intercept.go:1032`), so
the gap is more likely in the TTC fields 18c omits or orders differently than in the TNS
length framing alone.

## Problem

Failures from the CI log, grouped by symptom:

1. **A refusal ends the sqlplus session instead of the statement** (ORA-03113 then ORA-03114):
   `TestIntegration_BlockedStatementRefusesSQLPlus` (`blocked_integration_test.go:345`, `:347`),
   `TestIntegration_RepeatedStatementFromSQLPlusUnderReadOnly`
   (`refcursor_oci_integration_test.go:216`, `:218`). The injected refusal (error packet, or the
   break/reset marker exchange of `markers.go`) is not what an 18c OCI client accepts, so it
   drops the connection.
2. **JDBC thin sees `ORA-17401: Protocol violation [0, ]`**:
   `TestIntegration_BlockedStatementRefusesJDBCThin` (`blocked_integration_test.go:532`; the
   session logged `learned extra_tail_fields=[0]`), `TestIntegration_AsyncRefusalAgainstJDBCThin`
   both subtests (`async_refusal_integration_test.go:290`, and `:482` where the probe never
   reaches its idle point and the subtest runs out its 90 s), `TestIntegration_StatementTagFromJDBCThin`
   (`statement_tagging_integration_test.go:772`). In the async case JDBC fails at connect
   (`connect: code=17401`), so something dbbat rewrites in the login or the first exchanges
   (AUTH rewrite path, `relay_preauth.go`) is malformed for 18c JDBC.
3. **TNS capture**: `TestIntegration_TNSCapture` (`integration_test.go:237-242`) sends dbbat's
   own `buildTNSConnect` straight to 18c and gets a `Resend` (`tns.go:22`) instead of
   Accept/Refuse/Redirect. 18c asks the client to resend the connect (expected for some
   listener configurations); the test helper doesn't follow a Resend, and the proxy's own
   connect path must be checked for the same gap.
4. **Row and statement capture**: `TestIntegration_OCILongStatementIsRecordedWhole`
   (`oci_long_statement_integration_test.go:140-147`) records 7899 of 40610 bytes: the
   statement arrives in several fragments on 18c and the reassembly stops at the first one.
   `TestIntegration_OCIRowCaptureSurvivesALOBInTheSelectList` and
   `TestIntegration_OCIRowCaptureCarriesRealColumnNames` fail (about 60 s each, likely timeouts)
   in the v315 QueryResult / describe decoding (`intercept.go:1032`).
5. **REF cursor and statement tagging from OCI**: `TestIntegration_RefCursorFromSQLPlusUnderReadOnly`
   (`refcursor_oci_integration_test.go:106`, `:108`: REF cursor ids are not read out of the bind
   output, so the statement-less wide exec is forwarded undecoded),
   `TestIntegration_StatementTagFromOCIClient` (`statement_tagging_integration_test.go:504`:
   `tagged=0`), `TestIntegration_StatementTagFromOCIPLSQLCall` (`:588`: one untagged twin in
   `V$SQL`). The OCI exec layout the tagger and cursor learner parse differs on 18c.

The go-ora refusal test on 18c passes (`async_refusal_integration_test.go:412-424`), and the
sqlplus async quota refusal passes too: the protocol paths work for some clients, which points
at client-specific TTC fields rather than at the TNS envelope.

## Why a spec

- Over ~4 source files / ~150 lines: five independent failure families across the refusal,
  AUTH rewrite, TNS connect, statement reassembly, row capture, cursor learning and tagging paths.
- A cross-cutting change in the wire-protocol layer, each fix needing amd64 CI round trips to
  reproduce (the 18c image doesn't run on the arm64 Mac).
- Correctness hinging on authorization: refusals that kill the session, and statements recorded
  as a prefix "the gate enforced against rather than the statement", are access-control gaps.

## Proposal

Work family by family, each with a capture first:

1. **Capture fixtures from 18c** on CI: extend the existing fixture recorders
   (`capture_oci_fixtures_integration_test.go`, `ORACLE_CAPTURE_OCI_FIXTURES=1`) to run on the
   18c leg and upload the captured packets as a workflow artifact (a `workflow_dispatch` input),
   then commit them under the same fixture directory with an `18c` suffix. Every fix below
   gets a unit test replaying them, so it runs on the Mac.
2. **TNS Resend**: follow a Resend (re-send the Connect, bounded retries) in the test helper
   and verify the proxy's upstream connect path does the same. Unit test with a fake listener
   answering Resend then Accept.
3. **Refusals on 18c OCI and JDBC**: diff the error/marker sequence dbbat injects against what
   18c itself sends for a real server-side error to the same client (capture a real ORA-00942
   through the proxy in pass-through), and build the 18c variant. Unit tests on the fixtures.
4. **JDBC thin `ORA-17401` at connect**: diff the AUTH exchange with and without dbbat on 18c
   JDBC; fix the rewrite (or fall back to pass-through for that combination) so the login succeeds.
5. **Long statement reassembly, row capture, REF cursor ids, statement tags**: fix the v315
   parsers for the 18c field layout found in the fixtures; one fixture-based unit test each.
6. Run the full 18c leg green (`gh workflow run "Integration tests" --ref <branch>`), plus the two
   23ai legs, before closing. Document the 18c differences in `docs/oracle.md`.

## Open questions

- **Ship partial progress?** The families are independent. Recommended: yes, one commit per
  family, the spec done only when the whole leg is green. Trade-off: the nightly stays red until
  the last family lands.
- **Mark the 18c leg `continue-on-error` meanwhile?** Recommended: no, keep it red so the gap
  stays visible; it only runs nightly and doesn't gate PRs. Trade-off: a red nightly every night
  until the spec is done.

## Resolved open questions

- **Ship partial progress:** commit one fix per failure family; the spec is done only when the whole 18c leg is green. (autopilot: recommended answer)
- **`continue-on-error` on the 18c leg:** do not add it; keep the nightly red until fixed. (autopilot: recommended answer)

## Closing the issue

This spec closes #505. The implementing PR body **must** carry a `Closes #505` line so the
squash-merge closes it. When the spec is archived to `specs/done/`, verify the issue is closed
and, if the merge did not close it, close it by hand:

    gh issue close 505 --comment "Implemented by <PR or commit>; spec: <archived spec path>"
