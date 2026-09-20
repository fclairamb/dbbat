---
model: opus
effort: medium
---

# The Oracle statement tag never reaches the 64-bit OCI client, and the test that says so is red

## Goal

Make `DBB_QUERY_TAGGING_ORACLE=user` tag statements on the **64-bit** OCI
dialect the way it does on the 4-byte one, so
`TestIntegration_StatementTagFromOCIClient` passes against the client CI runs —
or, if the locator genuinely cannot certify that header, make the test say so
instead of asserting a claim that does not hold.

## Why

Found on 2026-09-21 while running the full Oracle integration suite against the
container client for
`2026-09-20-03-oracle-wide64-oci-refcursor-and-drive.md`. One test fails:

```
--- FAIL: TestIntegration_StatementTagFromOCIClient (30.31s)
    Error: "probe=1\ntagged=0\n" does not contain "tagged=1"
```

**It is not a regression.** The same run against the pre-dispatch commit
(`ec5bb8bf`, in a detached worktree, same client, same image) fails identically,
byte for byte. It has simply never been run against this client: CI would have
caught it, but the branch is unpushed — the same way the REF-cursor gap in
`2026-09-20-03` went unnoticed.

The cause is visible without measuring anything further. `stmtLenKind` names
**three** encodings the rewriter can put a new length into —
`stmtLenCompressed`, `stmtLenWideUB4` (the OCI 4-byte header's `sqlLen * 3`) and
`stmtLenVarLen` (OALL8's) — and the 64-bit OCI header's is none of them: it
declares a **plain byte count as a little-endian ub8** at offset 33, behind an
eight-byte sequence pad the 4-byte header spends one byte on (measured and
written down in `execWide64NoStatementCursor` and docs/oracle.md, "Cursor
re-execution"). `execSQLLengthWideField` keys on `body[3] == 0x01`, which this
header does not carry, so the locator falls through to the thin walk, cannot
certify the frame, and — exactly as designed — leaves the session **untagged**
start to finish.

So the behaviour is correct by the rule `DBB_QUERY_TAGGING_ORACLE` documents
("a client shape the locator cannot certify runs untagged start to finish and
logs why"); what is wrong is that the shape *is* now understood well enough to
certify, and that a test asserts otherwise. Tagging is off by default, so
nothing in production is mis-tagged; what is lost is the attribution the feature
exists for, on the thick-client family most likely to be running ad-hoc work.

## Implementation

1. Confirm the diagnosis before writing code: run the test with
   `ORACLE_TEST_OCI_CLIENT=container` and `DBB_LOG_LEVEL=debug`, and find the
   log line that says why the session decided not to tag
   (`decideStatementTagging`). It should name the locator, not the rewrite.
2. Add the fourth length kind — the 64-bit header's ub8 — to `stmtLenKind` and
   to `locateStatementRewrite`'s writer, and an `execSQLLengthWide64Field`
   beside `execSQLLengthWideField`. The offsets are already measured: options at
   17, cursor id at 21, the `fe x8` pointer sentinel at 25, the length at 33.
   **The length is the plain byte count, not three times it** — that difference
   is what would silently produce a third of a statement.
3. Key it on the session's learned dialect, never on sniffing: the exec header
   is a client frame, so the flag is `session.clientWide64Encoding`, the same
   one the re-execution reading uses (see `execNoStatementCursor`).
4. Hold it to the bar the 4-byte dialect is held to. The unit half is the
   rewrite survey (`ttc_statement_rewrite_survey_test.go`) plus a round-trip on
   a recorded 64-bit parse — `testdata/oci64_parse_execs.hex` already holds
   three, and re-encoding what the locator reads must reproduce the client's own
   bytes. The live half is the failing test, which must go green against the
   container client while `TestIntegration_StatementTagFromOCIClient` on the
   Instant Client stays green.
5. If step 2 turns out not to be certifiable — a shape the locator cannot
   relocate to the byte — then say that in the test instead: assert `probe=1`
   and *no* tag on this dialect, name the reason, and record it in
   docs/oracle.md next to Oracle's own tagging caveats. What must not stay is an
   assertion that is simply red.

## Files

- `internal/proxy/oracle/ttc_exec_statement.go` — `execSQLLengthField`,
  `execSQLLengthWideField`, and the 64-bit offsets already named there
- `internal/proxy/oracle/ttc_statement_rewrite.go` — `stmtLenKind` and the
  length writer
- `internal/proxy/oracle/statement_tagging.go` — `frameCarriesStatement`,
  `decideStatementTagging`
- `internal/proxy/oracle/statement_tagging_integration_test.go` —
  `TestIntegration_StatementTagFromOCIClient`
- `internal/proxy/oracle/testdata/oci64_parse_execs.hex` — three recorded
  64-bit parses to round-trip against
- `docs/oracle.md` — `DBB_QUERY_TAGGING_ORACLE`'s caveats
