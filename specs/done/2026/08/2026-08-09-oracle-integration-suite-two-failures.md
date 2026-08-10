# Oracle: two integration tests fail on `make test-e2e-oracle`

**No GitHub issue filed yet — one should be.**

## Goal

Get `make test-e2e-oracle` green again. Two tests fail, both for reasons of
their own rather than a proxy regression, and while they fail the suite gives no
signal at all.

## Why

Observed on 2026-08-09 with `ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim`
(everything else in the suite passed, 271s):

1. `TestIntegration_ProxyPassthrough` — fails during setup at
   `integration_test.go:267` with `a grant must reference a grant definition`.
   The test still builds a `store.Grant` the old way; grants have carried their
   shape on a `GrantDefinition` since the definitions refactor, so this is stale
   test code, not a proxy fault.
2. `TestIntegration_ConnCheckOracleLogin/wrong_password_is_an_auth_failure` —
   the connectivity check classifies a wrong password as `db_handshake_failed`
   instead of `db_auth_failed` (`the target was reachable but the database
   handshake failed: EOF`). Oracle 23ai appears to drop the connection rather
   than return an ORA-01017 the classifier can read, so either the classifier
   needs the 23ai shape or the expectation needs revisiting.

Neither is caused by the cursor re-execution work landed the same day — the
first fails before any traffic, the second never opens a proxy session — but
both were found by it and nobody is tracking them.

## Implementation

- For (1): construct the grant through a `GrantDefinition` the way the other
  integration tests do, and check whether sibling suites (mysql, mssql, mongodb)
  carry the same stale construction.
- For (2): capture what 23ai actually sends on a bad password through
  `internal/proxy/conncheck` and extend the Oracle classifier, or — if the
  server really does just close — record that as the expected code for 23ai and
  keep `db_auth_failed` for versions that answer.
- Run `ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim make test-e2e-oracle` to
  verify; it takes ~5min and starts its own containers.

Key files: `internal/proxy/oracle/integration_test.go`,
`internal/proxy/oracle/conncheck_integration_test.go`,
`internal/proxy/conncheck/`.
