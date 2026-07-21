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

## Implementation Plan

Mirror commit `1619a95` (the PG proxy fix) in `internal/proxy/mysql/server.go`:

1. Add a `listenerMu sync.Mutex` field guarding `listener` (with a doc comment
   explaining Start writes it while Addr/Shutdown read it concurrently).
2. Add `setListener`/`getListener` helpers that lock/unlock the mutex.
3. In `Start`, replace `s.listener = listener` with `s.setListener(listener)`.
4. In `Addr` and `Shutdown`, read the listener via `getListener()` instead of
   touching `s.listener` directly.
5. Run `gofmt`, `make build-binary`, `make lint`, `make test`,
   `go vet -tags integration ./...`, and a MySQL integration test under
   `-race` that exercises `Addr()` to confirm race-cleanliness.

Oracle/MongoDB audit is out of scope here; capture as a separate follow-up if
the same pattern exists there.
