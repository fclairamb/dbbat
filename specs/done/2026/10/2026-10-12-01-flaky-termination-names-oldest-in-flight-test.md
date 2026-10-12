---
model: sonnet
effort: medium
---

# `TestSession_TerminationNamesOldestInFlight` is flaky: it reads the `error` column before the teardown writes it

## Problem
On Renovate PR #417 (actions/download-artifact v8.0.2, no Go change) the `test` job failed on
`require.NotNil(t, oldest.Error)` (`internal/proxy/postgresql/session_bookkeeping_test.go:632`)
and passed on a plain re-run (Actions run 38309382757). `main` was green (a3b38f34).

Suspected cause (not verified): the test inserts both rows up front with `insertRow`
(`session_bookkeeping_test.go:589-613`), so `h.awaitConnectionQueries(t, 2)`
(`session_bookkeeping_test.go:200`) is satisfied by the row count alone. It can return before
`persistTerminatedQuery` (`internal/proxy/postgresql/session.go:1484`) has finished writing the
`error` column through `logQuery`, if that write is asynchronous. The assertions then read a
row that has not been completed yet.

## Proposal
1. Reproduce: `go test -race -count=200 -run TestSession_TerminationNamesOldestInFlight ./internal/proxy/postgresql/`
   (add `-cpu 1,2` or run under load if it does not fail).
2. Confirm in `session.go` (`logQuery`, `persistTerminatedQuery`) whether completion is
   asynchronous. If it is, fix the test: replace the single `awaitConnectionQueries` read with a
   bounded `require.Eventually` that re-lists the rows until both `Error` columns are non-nil,
   then assert (`session_bookkeeping_test.go:629-640`). No sleeps or retries.
3. If the teardown turns out to have a real ordering bug (rows completed after the session is
   reported done), fix it in `session.go` instead and keep the test strict.
4. Check the other `awaitConnectionQueries` callers (`:335`, `:371`, `:431`) for the same
   pattern, where the row exists before the column they assert on. Fix any found.

## Tests
- `TestSession_TerminationNamesOldestInFlight`: passes at `-race -count=200`; asserts the oldest
  row has the limit text and the other row has the aborted text without it.
- Negative case: a pending entry with no persisted row must not make the wait pass vacuously
  (the `Eventually` condition requires both `Error` values, not just the row count).
- Whole package: `go test -race ./internal/proxy/postgresql/`.

## To verify
- Whether `logQuery` writes asynchronously (the `awaitConnectionQueries` comment says
  "Persistence is asynchronous by design").
