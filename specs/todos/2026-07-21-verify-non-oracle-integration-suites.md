# Verify the MongoDB / MySQL / PostgreSQL integration suites actually run

## Goal

Run the `-tags integration` suites of `internal/proxy/mongodb`,
`internal/proxy/mysql` and `internal/proxy/postgresql` against real databases
and fix whatever has rotted, the way the Oracle suite was just fixed.

## Why

`go vet -tags integration ./...` is now green repo-wide and enforced in CI
(added while fixing the Oracle integration build), so every tagged suite
*compiles*. That is a much weaker guarantee than "works".

Concrete evidence of rot found in passing: `internal/proxy/mongodb/integration_test.go`
had a dozen calls to `client.Server(...)` — a repo-wide `Database` → `Server`
rename had rewritten `mongo.Client.Database()` into a method that does not
exist. Nobody noticed because the tagged build was never compiled. A rename
that mangles source that badly may well have mangled semantics elsewhere in
those files without breaking compilation.

The Oracle suite additionally needed real fixes that a compile check would
never have caught: `CreateServer` now requires a 32-byte encryption key, and
`buildTNSConnect` had to announce TNS 313 (315+ makes a 23ai listener drop the
connection).

## Implementation

- For each of the three packages, run the tagged suite with Docker available:
  `go test -tags integration -timeout 40m ./internal/proxy/mongodb/` etc.
- Expect the same classes of breakage the Oracle suite had: store constructor
  signatures that grew parameters (encryption keys, `config.DumpConfig`),
  `CreateGrant` now taking a `*store.Grant`, testcontainers API drift.
- On Apple Silicon, check each default image has an arm64 build; follow the
  `ORACLE_TEST_IMAGE` / `ORACLE_TEST_SERVICE` precedent (see
  `internal/proxy/oracle/integration_test.go` and the "Integration tests"
  section of `docs/oracle.md`) rather than hardcoding.
- Once a suite passes live, document how to run it next to the protocol notes
  in `docs/mongodb.md`, `docs/mysql.md`.
- Consider a scheduled (not per-PR) CI job that actually *runs* the tagged
  suites, since the vet step only proves they compile.

No GitHub issue exists for this yet — one should be filed.
