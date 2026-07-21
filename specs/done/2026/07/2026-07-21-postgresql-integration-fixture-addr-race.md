# Fix the Server.Addr/Start data race in the PostgreSQL integration fixture

## Goal

Make `internal/proxy/postgresql/integration_test.go`'s `setupFixtureWith`
race-clean so the PG integration suite passes under `go test -tags integration -race`.

## Why

Running any PG integration test with `-race` (e.g.
`go test -tags integration -race -run TestIntegration_ExtendedProtocol ./internal/proxy/postgresql/`)
reports a data race in the fixture, not in production code:

- `setupFixtureWith` launches `server.Start()` in a goroutine and, concurrently,
  polls `server.Addr()` inside an `assert.Eventually`.
- `Server.Start` (`server.go:77`) writes the listener field while `Server.Addr`
  (`server.go:116`/`120`) reads it — an unsynchronised read/write on both the
  struct field and the underlying `net.TCPListener`.

This is pre-existing (reproduces on `main` in tests untouched by recent work,
e.g. `TestIntegration_ReadOnlyGrant_BlocksWrite`) and blocks `-race` coverage of
the suite.

## Implementation

- Guard the listener in `Server` with a mutex (or store it in an
  `atomic.Pointer`), so `Start` and `Addr` synchronise. Key file:
  `internal/proxy/postgresql/server.go` (`Start`, `Addr`).
- Alternatively/additionally, have `setupFixtureWith` wait on a "listening"
  signal from `Start` before polling `Addr`, instead of racing on it.
  Key file: `internal/proxy/postgresql/integration_test.go` (`setupFixtureWith`).
- Once clean, consider adding `-race` to the integration test invocation in the
  Makefile / CI.

No GitHub issue exists for this yet — one should be filed.

## Implementation Plan

1. Guard `Server.listener` in `internal/proxy/postgresql/server.go` with a
   `sync.Mutex`. Add `setListener`/`getListener` helpers so `Start` writes the
   listener under the lock and `Addr` reads it under the lock. This removes the
   unsynchronised read/write on the struct field. The underlying
   `net.TCPListener` is itself safe for concurrent `Addr()`/`Accept()`; the race
   is on the Go struct field, which the mutex fixes.
2. `Shutdown` also touches `s.listener` — read it once under the lock before
   closing, so shutdown is consistent with the guarded field.
3. Leave `setupFixtureWith` as-is (its `Eventually` poll on `Addr()` becomes
   safe once the field is guarded).
4. QA under `-race`: `go test -tags integration -race -run
   TestIntegration_ExtendedProtocol ./internal/proxy/postgresql/` must be
   race-clean. Plus `make build-binary`, `make lint`, `make test`, and
   `go vet -tags integration ./...`.
