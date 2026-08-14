# The panic guards are open-coded in two packages because `shared` is not a leaf

## Goal

Give the goroutine panic guards (`RunRelay`, `RunGuarded`, `RunWatchdog`,
`RunMaintenance`, `LogGoroutinePanic`) a home that every package can import, so
the two current copies of the recover can be deleted.

## Why

`2026-08-13-12` guarded every detached goroutine in the process, but two of them
could not use the helpers and had to open-code the recover instead:

- `internal/cache/auth.go` — `AuthCache.guardedCleanup`. `internal/proxy/shared`
  imports `internal/cache`, so importing it back is a cycle.
- `internal/proxy/upstream/kubernetes_conn.go` — `watchErrorStream`. `shared`
  imports `internal/proxy/upstream` (the connectors live there), same problem.

Both copies carry a comment saying "keep the two in step", which is exactly the
kind of instruction that stops being true. The log message, the attribute names
and the stack handling are duplicated by hand, so a future change to `logPanic`
silently applies to thirteen call sites and not to these two.

The cause is that `panics.go` lives in `internal/proxy/shared`, which is a big
package sitting mid-graph, not a leaf. Nothing in the guards themselves depends
on anything in `shared`: they need `context`, `log/slog`, `runtime/debug`,
`errors` and `fmt`.

## Implementation

- Move `internal/proxy/shared/panics.go` (and `panics_test.go`, and the
  `RunMaintenance` half of `detached_writes_test.go`) into a new leaf package —
  `internal/safe` reads well, `internal/goroutine` is more literal. It must
  import only the standard library, and a test should assert that (`go list
  -deps` in CI, or simply keep it obvious).
- Decide whether `shared` re-exports the names as thin aliases (`var RunRelay =
  safe.RunRelay`) or whether the ~15 call sites are updated to the new import.
  Updating them is cleaner and the churn is mechanical; aliases would leave two
  spellings of the same thing, which is the problem this todo is about.
- Delete `AuthCache.guardedCleanup`'s hand-rolled recover in favour of
  `safe.RunMaintenance`, and `watchErrorStream`'s in favour of
  `safe.RunGuarded`. The latter also removes the `//nolint:contextcheck` at
  `internal/proxy/upstream/kubernetes.go:577`, since the guard would then take
  the context the helper is given rather than a fresh one.
- `LogMsgRelayPanic` and friends are referenced by name in
  `docs/`-adjacent prose and in tests; grep before renaming anything.

No GitHub issue yet — one should be filed.
