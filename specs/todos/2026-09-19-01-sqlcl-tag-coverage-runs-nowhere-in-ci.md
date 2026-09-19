---
model: opus
effort: small
---

# SQLcl is the one tagging client CI cannot run

## Goal

Decide whether `TestIntegration_StatementTagFromSQLcl` should run in CI, and
either wire it up or write down why it never will.

## Why

`2026-09-16-09` added live tag coverage for the `compressed`/`bare` shape from
two clients: ojdbc thin (jar-driven) and SQLcl. The nightly Oracle legs of
`.github/workflows/integration.yml` now fetch a pinned `ojdbc11` from Maven
Central, so the jar-driven half runs there — but SQLcl is a ~100 MB download
from Oracle's own site (no Maven coordinate, a click-through on some mirrors),
so `TestIntegration_StatementTagFromSQLcl` skips on every runner.

That is the exact shape of the hole `2026-08-12-05` closed for sqlplus: a test
that is green by proving nothing, on every machine that isn't a developer's.
The saving grace here is that SQLcl is the *same wire shape* as the jar — it is
ojdbc thin underneath — so what CI loses is client-version coverage rather than
a shape. That may well be the right trade; it should be a recorded decision
rather than an accident of what was easy to fetch.

## Implementation

Two credible outcomes, and the work is mostly deciding between them:

- **Wire it up.** `oracle-actions/run-sql` and the `sqlcl` npm package
  (`@oracle/sqlcl`, if it is still published) are the two routes that need no
  click-through. Cache the unpacked tree with `actions/cache@v6.1.0`, export
  `ORACLE_TEST_SQLCL`, and let `oracleTestSQLcl` find it. Cost is a download
  per nightly run on three legs.
- **Write it off.** Say in `docs/oracle.md` that SQLcl is a developer-machine
  client by design, note the jar version CI pins as the thing that actually
  gates the `bare` shape, and leave the test as the local escape hatch it is.
  If this is the choice, `oracleTestSQLcl` should probably gain the
  `ORACLE_TEST_REQUIRE_*` treatment anyway, so a developer who *does* set
  `ORACLE_TEST_SQLCL` gets a failure rather than a skip when it stops working.

Either way the relevant files are
`internal/proxy/oracle/statement_tagging_integration_test.go` (the test and
`oracleTestSQLcl`), `.github/workflows/integration.yml` (the Oracle job), and
the tagging section of `docs/oracle.md`.
