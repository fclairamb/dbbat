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
