---
model: opus
effort: medium
---

# The gate still *scans* for a 64-bit OCI statement, though the header now says where it is

## Goal

Let `decodeExecStatementText` read the 64-bit OCI exec header the way
`locateStatementRewrite` now does, so the statement the gate enforces against —
and the one `queries` records — on that dialect comes from the length its own
header declares rather than from the 40-70 window scan and the keyword scan
behind it.

## Why

Found on 2026-09-22 while implementing
`2026-09-21-02-oracle-statement-tag-never-reaches-the-64-bit-oci-client.md`,
which added `execSQLLengthWide64Field`: the reading exists now, and only the
*rewriter* uses it.

`execSQLLengthField` is still the 4-byte-or-thin walk (it is
`execSQLLengthFieldFor(body, false)`), and it takes no dialect because its
callers — `execSQLLength` → `decodeExecStatementAt` → `decodeExecStatementText`
— have none to give. On a 64-bit OCI frame that walk refuses: `body[3]` is `0x00`
so the wide reading bails, and the thin walk then reads `body[5]` (`0x7b`, the
low byte of this header's ub4 error number) as a compressed-int size of 123 and
gives up. So `decodeExecStatementText` returns false and
`decodePiggybackExecSQL` falls through to the offset window and then to
`findSQLInPayload`.

Observed live, in the debug log of that spec's own before/after run: the
statements *are* recorded, so this is not a bypass — but they are recorded by the
mechanism `ttc_exec_statement.go`'s own header comment describes as the thing it
replaced, whose measured failure rate on the recorded corpus was **48 of 137
execute ops misread**, three of them as enforcement failures
(`ALTER SESSION SET CURRENT_SCHEMA=…` read as `SET …`, so neither `ALTER` nor a
write keyword fired). Those numbers are the thin/4-byte corpus, not this dialect,
which is exactly the point: nobody has measured it here, because the precise
decode never ran here.

It is a correctness risk rather than a correctness bug today, which is why it is
its own spec instead of part of the tagging one: the tag change was verifiable
end to end against a live server, and widening the decode is a behaviour change
on the gate's hot path for a whole client family that wants its own measurement.

## Implementation

1. Thread the dialect down the decode path the way the re-execution reading
   already threads it: `decodePiggybackExecSQL` and `decodeExecSQL` both take
   `wide64` already and pass it to `execNoStatementCursor`, so the missing link is
   `decodeExecStatementText(ttcPayload, wide64)` →
   `decodeExecStatementAt(body, wide64)` → `execSQLLength(body, wide64)` →
   `execSQLLengthFieldFor`. Every call site outside the package's tests is in
   `ttc_decode.go` and already holds the flag.
2. Watch the one caller that is not a decode: `decodeExecSQL`'s
   `decodeExecStatement(ttcPayload)` two-liner, and `stapledStatements` in
   `session.go` (which already has `s.clientWide64Encoding`).
3. Measure it the way the 4-byte reading was measured, or the change is not
   worth making: extend `sql_extraction_survey_test.go` to cover the 64-bit
   frames (`testdata/oci64_parse_execs.hex`, plus whatever
   `capture_oci_fixtures_integration_test.go` can be asked for), and state the
   before/after counts — how many of those frames the window scan reads whole,
   and how many the header-anchored decode does.
4. Live half: `make test-e2e-oracle` with `ORACLE_TEST_OCI_CLIENT=container`,
   with attention to the tests that assert on recorded statement *text*
   (`recordedStatements`), since a decode that becomes precise changes what a
   session records — for the better, but not identically.
5. If the survey shows the window scan already reads every recorded 64-bit frame
   whole, say so in `docs/oracle.md` and close this as measured-not-needed rather
   than shipping a hot-path change for a hole that is not there.

## Files

- `internal/proxy/oracle/ttc_exec_statement.go` — `execSQLLength`,
  `decodeExecStatementAt`, `decodeExecStatementText`, and
  `execSQLLengthFieldFor`, which already has the dialect parameter
- `internal/proxy/oracle/ttc_decode.go` — `decodePiggybackExecSQL`,
  `decodeExecSQL`, both of which already hold `wide64`
- `internal/proxy/oracle/session.go` — `stapledStatements`
- `internal/proxy/oracle/sql_extraction_survey_test.go` — the before/after
  measurement
- `docs/oracle.md` — the exec-decode section, and "Two OCI encodings, not one"
