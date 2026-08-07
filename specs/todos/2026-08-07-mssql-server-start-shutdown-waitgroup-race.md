# SQL Server proxy: `wg.Add` races `wg.Wait` between Start and Shutdown

## Goal

Make `internal/proxy/mssql/server.go` race-clean, so `go test -race
./internal/proxy/mssql/...` is a usable signal for the concurrency work that
package is full of.

## Why

Found while running the mssql suite under `-race` for the ATTENTION-during-a-
hold spec. It is **not** caused by that work — the report names only
`Server.Start` and `Server.Shutdown`, neither of which that change touched.

`Server.Start`'s accept loop calls `s.wg.Add(1)` for each accepted connection
(`server.go:164`), while `Shutdown` calls `s.wg.Wait()` from a goroutine
(`server.go:213`). `sync.WaitGroup` documents that an `Add` which takes the
counter up from zero must *happen before* a concurrent `Wait`; when it does not,
the race detector flags it — correctly, because `Wait` may return before the
connection it was supposed to wait for is registered.

Reproduce (roughly one hit in a few hundred, enough to redden a whole `-race`
run because Go marks every test in the binary as failed once a race fires):

```bash
go test ./internal/proxy/mssql/ -count=200 -race -run TestServerStartAndShutdown
```

`make test` does not pass `-race`, which is why this has never been visible.

Since a race anywhere in the binary fails every test in it, this currently makes
`go test -race ./internal/proxy/mssql/...` unusable as a gate; the workaround is
`-skip TestServerStartAndShutdown`, which is clean.

## Implementation

The connection counter has to be registered under whatever also decides that
shutdown has begun. Options, cheapest first:

1. **Hold the counter open for the lifetime of the accept loop.** `wg.Add(1)`
   once before the loop and `wg.Done()` when it exits, so the counter never
   drops to zero while `Start` is still running and per-connection `Add`s are
   never the zero→one transition.
2. **Guard accept-side registration with the shutdown state.** Take a mutex (or
   re-check `s.shutdown`) around `wg.Add`, and have `Shutdown` close the
   listener and mark the server stopped under the same mutex before it waits.

Check the other four proxies for the same shape while in there — the
`shutdown`-channel + `WaitGroup` pattern is copied between them
(`internal/proxy/*/server.go`).

A regression test is awkward (it is a probabilistic race), so the real
verification is `-count=200 -race` on the existing
`TestServerStartAndShutdown`, plus making `-race` part of what CI runs for this
package.

Files: `internal/proxy/mssql/server.go`, `internal/proxy/mssql/session_test.go`
(`TestServerStartAndShutdown`), `.github/workflows/`.

No GitHub issue exists yet — one should be filed.
