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
