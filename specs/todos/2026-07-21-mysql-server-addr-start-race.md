# Fix the Server.Addr/Start data race in the MySQL proxy server

## Goal

Guard `internal/proxy/mysql/server.go`'s `listener` field so concurrent
`Addr()`/`Start()`/`Shutdown()` access is race-clean, matching the fix applied to
the PostgreSQL proxy.

## Why

`internal/proxy/mysql/server.go` has the same unsynchronised pattern the
PostgreSQL proxy had: `Start` writes `s.listener` while `Addr` and `Shutdown`
read it without a lock. If a MySQL integration fixture launches `Start()` in a
goroutine and polls `Addr()` (as the PG fixture does), it will trip `-race`.

The PG fix landed in `internal/proxy/postgresql/server.go` (commit series for
spec `2026-07-21-postgresql-integration-fixture-addr-race.md`): a `sync.Mutex`
guarding the listener with `setListener`/`getListener` helpers.

## Implementation

- Mirror the PG fix in `internal/proxy/mysql/server.go`: add `listenerMu
  sync.Mutex`, write via a `setListener` helper in `Start`, read via
  `getListener` in `Addr` and `Shutdown`.
- Verify under `-race` with a MySQL integration test that exercises `Addr()`.
- Consider the same audit for `oracle` and `mongodb` proxy servers.

No GitHub issue exists for this yet — one should be filed.
