# Repair the Oracle integration fixture's grant creation

## Goal

Make `make test-e2e-oracle` able to reach its assertions again: the suite's
`TestIntegration_ProxyPassthrough` fails at setup with **"a grant must
reference a grant definition"**, so nothing it covers is actually being tested.

While in there, add the Oracle half of the blocked-statement coverage: a
`read_only` grant refusing a write and a `block_ddl` grant refusing a
`CREATE TABLE` must each leave a `queries` row with a non-null `error` (the
PostgreSQL half of that is already asserted in
`internal/proxy/postgresql/integration_test.go`).

## Why

Since grants became instances of definitions (`85209da`), `store.CreateGrant`
requires `GrantDefinitionID` — an inline `Definition` on the grant struct is
not enough, it answers `ErrGrantDefinitionRequired`. Every integration fixture
written before that change still passes only `Definition`.

The PostgreSQL suite had exactly the same breakage and was repaired while
implementing `2026-08-09-log-blocked-statements-pg-oracle`; the Oracle one was
deliberately left alone (it was flagged as a known, out-of-scope failure). It
should not stay broken: a suite that fails at setup is a suite whose assertions
have silently stopped running.

Second, unrelated blocker seen on the same run: on a machine that already has
an Oracle XE instance, the test container dies at startup with
`ORA-00442: Oracle Database Express Edition (XE) single instance violation`
(container exit code 186), so *every* Oracle integration test fails locally
regardless of the fixture. Worth a note in the suite's docs, or a switch to
`gvenzl/oracle-free` (23ai) as the default image, which does not carry the
single-instance restriction.

No GitHub issue yet — file one when picking this up.

## Implementation

- `internal/proxy/oracle/integration_test.go`, `setupFixture`-equivalent around
  line 267: create the definition first, then reference it by uid. The shape
  that works is `createGrantWithControls` in
  `internal/proxy/postgresql/integration_test.go` — note `CreatedBy` must be
  set, `grant_definitions_created_by_fkey` enforces it, and
  `DurationSeconds` is an `int64`.
- Check `internal/proxy/oracle/conncheck_integration_test.go` for the same
  pattern.
- Then add the refusal assertions. The Oracle suite currently has no
  client-through-proxy SQL path at all (`TestIntegration_ProxyPassthrough`
  stops at the TNS Connect handshake), so this needs a `go-ora` client dialing
  the proxy — the pieces are in `internal/proxy/oracle/session.go`'s O5LOGON
  termination and the harness notes in `docs/oracle.md`.
- The behaviour under test already exists and is unit-covered:
  `internal/proxy/oracle/blocked_persist_test.go` drives `handleOALL8` and a
  cursor re-execution through a real store, including the query-chain check.

## Also on the same run

`TestIntegration_ConnCheckOracleLogin/wrong_password_is_an_auth_failure` was
reported as misclassifying its failure. It could not be confirmed here — the
container never started — so re-check it once the fixture and the image issue
are sorted.
