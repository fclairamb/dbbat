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

## Implementation Plan

1. **Run what exists.** `go test -tags integration ./internal/proxy/mongodb/`
   and `./internal/proxy/mysql/` live on Docker (arm64, OrbStack), fix whatever
   breaks.
2. **PostgreSQL has no tagged suite at all** — `internal/proxy/postgresql/`
   contains zero `//go:build integration` files, so "run the PostgreSQL
   integration suite" is impossible as written. Write one, mirroring the MySQL /
   MongoDB fixtures: PG storage container + PG upstream container + started
   proxy, `PGPROXY_TEST_IMAGE` env override for the upstream image. Cover
   auth (password + `dbb_` API key + wrong password), a query round-trip with
   query-log/result capture, extended-protocol (prepared statement) capture, a
   `read_only` grant blocking a write, `block_ddl` blocking a DDL, TLS
   termination at the proxy, and per-session dump files.
3. `postgresql.Server` has no `Addr()` accessor (MySQL and MongoDB both do), so
   a `:0` listen address can't be discovered by a test — add it, mirroring
   `internal/proxy/mysql/server.go`.
4. **Document how to run each suite** next to the protocol notes in
   `docs/mysql.md` and `docs/mongodb.md`, plus a PostgreSQL equivalent, matching
   the "Integration tests" section added to `docs/oracle.md`.
5. **Scheduled CI job** that actually *runs* the tagged suites (nightly cron,
   not per-PR — they take minutes and need Docker), since the existing vet step
   only proves they compile.
6. QA: `make build-binary`, `make lint`, `make test`,
   `go vet -tags integration ./...`, then the three tagged suites.
