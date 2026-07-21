# Fix the Server.Addr/Start data race in the Oracle and MongoDB proxy servers

## Goal

Guard the `listener` field in `internal/proxy/oracle/server.go` and
`internal/proxy/mongodb/server.go` so concurrent `Addr()`/`Start()`/`Shutdown()`
access is race-clean, matching the fix applied to the PostgreSQL and MySQL
proxies.

## Why

Both servers have the same unsynchronised pattern the PostgreSQL and MySQL
proxies had: `Start` writes `s.listener` (oracle L74, mongodb L92) while `Addr`
(oracle L146, mongodb L123) and `Shutdown` (oracle L112, mongodb L132) read it
without a lock. If an integration fixture launches `Start()` in a goroutine and
polls `Addr()`, it will trip `-race`.

The pattern was fixed for PostgreSQL in commit `1619a95` and for MySQL in the
`2026-07-21-mysql-server-addr-start-race.md` spec: a `sync.Mutex` guarding the
listener with `setListener`/`getListener` helpers used in `Start`/`Addr`/`Shutdown`.

## Implementation

- Mirror the PG/MySQL fix in `internal/proxy/oracle/server.go` and
  `internal/proxy/mongodb/server.go`: add `listenerMu sync.Mutex`, write via a
  `setListener` helper in `Start`, read via `getListener` in `Addr` and
  `Shutdown`.
- Verify under `-race` with an integration test that exercises `Addr()`.

No GitHub issue exists for this yet — one should be filed.
