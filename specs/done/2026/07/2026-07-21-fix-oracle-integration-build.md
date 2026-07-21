# Fix the broken `-tags integration` build in internal/proxy/oracle

## Goal

Make `go vet -tags integration ./internal/proxy/oracle/` (and therefore
`go test -tags integration ./internal/proxy/oracle/`) compile again.

## Why

The Oracle integration suite does not build on `main` today, so every
integration-tagged Oracle test — including the new connectivity-check login case
in `internal/proxy/oracle/conncheck_integration_test.go` — is dead weight: it can
never be executed until the package compiles. Two independent breakages, both
pre-existing and unrelated to each other:

1. `internal/proxy/oracle/integration_test.go:80` calls `port.Int()`, but the
   testcontainers-go version now in `go.mod` returns a
   `github.com/moby/moby/api/types/network.Port`, which has no `Int()` method
   (it exposes `Port() string`). API drift from a dependency bump that was never
   compiled against the integration tag.
2. `internal/proxy/oracle/integration_test.go:113` calls `buildTNSConnect`,
   which no longer exists anywhere in the package.

Because the tagged build is never exercised by `make test` or CI, both went
unnoticed.

## Implementation

- Replace `port.Int()` with `strconv.Atoi(port.Port())` (or whatever the pinned
  testcontainers-go release offers — check the vendored API rather than
  guessing).
- Find what replaced `buildTNSConnect`. `internal/proxy/oracle/tns.go` and
  `connect_descriptor.go` hold the current Connect-descriptor builders; the
  caller at `integration_test.go:113` is building a TNS Connect packet for a
  raw-socket test.
- Then actually run the suite once with Docker available:
  `go test -tags integration -run TestIntegration_ConnCheckOracleLogin ./internal/proxy/oracle/`
  and confirm the new connectivity-check assertions (good password → `ok`,
  wrong password → `db_auth_failed`, unknown service → `db_handshake_failed`)
  hold against a real Oracle.
- Consider adding a compile-only CI step (`go vet -tags integration ./...`) so
  the tagged build cannot rot again silently.

No GitHub issue exists for this yet — one should be filed.

## Implementation Plan

1. **`port.Int()` drift** — `container.MappedPort` now returns
   `github.com/moby/moby/api/types/network.Port`, whose API is
   `Num() uint16` / `Port() string` / `Proto()`. Replace `port.Int()` with
   `int(port.Num())` in `startOracleContainer` (`integration_test.go:80`). No
   `strconv` needed — `Num()` is already numeric and cannot fail.

2. **Missing `buildTNSConnect`** — the helper used to live in
   `internal/proxy/oracle/upstream_auth.go` and was deleted in `3a27833`
   ("Oracle terminated auth — go-ora end-to-end working"), when upstream
   connection setup moved to go-ora. Nothing in the non-test package builds a
   TNS Connect payload any more, so the helper belongs to the test that uses it.
   Re-add it as a test-only helper in `integration_test.go`, restoring the
   58-byte TNS Connect header from the pre-`3a27833` version (version 315,
   compatible 300, SDU 8192, TDU 65535, protocol characteristics `0x8001`,
   data length + data offset, connect flags `0x41`/`0x41`) and wrapping the
   service name in a real `(DESCRIPTION=…(CONNECT_DATA=(SERVICE_NAME=…)))`
   descriptor so the listener has something valid to parse. Both call sites
   (`:112` and `:199`) keep passing a bare service name, so their code is
   unchanged.

3. **Verify** — `go vet -tags integration ./internal/proxy/oracle/...` and a
   compile-only `go test -tags integration -run XXX ./internal/proxy/oracle/...`
   must be clean; `conncheck_integration_test.go` compiles as a side effect.
   Keep the default build green (`make build-binary`, `make lint`, `make test`).
   Executing the suite for real needs Docker plus an Oracle image pull, which
   may not be available here — the acceptance criterion is a clean tagged
   compile.

4. **Stop the rot** — add a compile-only `go vet -tags integration ./...` step
   to CI so the tagged build cannot silently break again.
