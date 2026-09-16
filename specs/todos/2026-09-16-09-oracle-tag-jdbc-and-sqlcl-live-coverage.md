---
model: opus
effort: medium
---

# The Oracle per-user tag is proven live on three client shapes, not five

## Goal

Get `DBB_QUERY_TAGGING_ORACLE=user` verified against a real 23ai from **ojdbc
thin** and **sqlcl**, the two clients the tagging integration suite currently
cannot drive on a developer machine.

## Why

`2026-09-16-08` shipped the TTC statement rewriter. Its corpus test locates and
round-trips **159 of 159** statement frames across all five recorded client
shapes, and `statement_tagging_integration_test.go` puts the tag on a real
`gvenzl/oracle-free:23-slim` and reads it back out of `V$SQL` for three of them:

- go-ora (`compressed`/`clr-short`) — green
- python-oracledb thin (`compressed`/`clr-short`) — green
- sqlplus / OCI (`wide-ub4`/`clr-short`) — green

**ojdbc thin and DBeaver are the fourth shape** (`compressed`/**`bare`** — no CLR
prefix at all, the header field is the statement's only declared length), and no
live run has exercised it. It is covered at the corpus level only.
`TestIntegration_StatementTagFromPythonThin`'s JDBC sibling does not exist
because `TestIntegration_BlockedStatementRefusesJDBCThin` needs
`ORACLE_TEST_OJDBC_JAR` (or an ojdbc jar on `CLASSPATH`), which was absent on the
machine the feature was built on.

That matters more here than it would for a read-only gate: the `bare` shape is
the one where the length field in the header is the *only* copy of the
statement's length, so a second copy hiding somewhere else in an ojdbc frame
would be caught by nothing dbbat currently runs. The corpus proves the frames
dbbat recorded; a live run proves the frames ojdbc sends today.

## Implementation

- Add `TestIntegration_StatementTagFromJDBCThin` next to the two client tests in
  `internal/proxy/oracle/statement_tagging_integration_test.go`, modeled on
  `TestIntegration_BlockedStatementRefusesJDBCThin`
  (`blocked_integration_test.go`): `exec.LookPath("java")`,
  `oracleTestOJDBCJar(t)`, a small `Tag.java` that runs a marked statement and
  then reads `V$SQL`, skipping when no jar is available.
- Do the same for sqlcl if the harness can reach one; sqlcl is a JDBC thin client
  under the hood, so it is the same wire shape and mostly buys client-version
  coverage.
- Make CI actually run it: `.github/workflows/integration.yml` already sets
  `ORACLE_TEST_REQUIRE_OCI_CLIENT` on the Oracle legs, so the jar should be
  fetched and `ORACLE_TEST_OJDBC_JAR` exported the same way, with the new test
  failing rather than skipping when the variable is set.
- While there: assert `V$SQL` shows **one** SQL_ID for a statement executed many
  times from the JDBC client, which is the per-user-cardinality claim on a client
  that prepares and re-executes rather than re-parsing.
