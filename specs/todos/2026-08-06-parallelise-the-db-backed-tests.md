# Parallelise the DB-backed tests now that each one owns its database

## Goal

Turn on `t.Parallel()` for the `internal/api` and `internal/store` tests that
are still opted out of it, and delete the `paralleltest` exclusions and
`//nolint:paralleltest` comments that justified the opt-out.

## Why

Those opt-outs all cite the same reason — "shares one PostgreSQL container and
truncates tables between tests", "shared database state". That reason no longer
holds: `setupTestServer` (`internal/api/password_reset_test.go`) and
`setupTestStore` (`internal/store/store_test.go`) now hand every test a private
database cloned from a migrated template, and the `DELETE FROM <table>` cleanup
that made tests clobber each other is gone.

So the comments are now wrong about why the code looks the way it does, and the
suites are serialised for a hazard that was removed. Both are worth fixing, and
the second one buys back wall-clock time in CI.

No GitHub issue for this — file one if it is picked up.

## Implementation

- `.golangci.yml`, `exclusions.rules`: drop the `paralleltest` entries for
  `internal/store/.*_test\.go`, `internal/api/password_reset_test\.go`,
  `internal/api/approvals_test\.go` and `internal/api/stream_approvals_test\.go`
  (and the stale "truncates tables between tests" comment above the last two).
- `internal/api/connection_dump_test.go`: drop the
  `//nolint:paralleltest // shared database state` comments and add
  `t.Parallel()`.
- Then let the linter point at the rest, add `t.Parallel()` file by file, and
  run each package with `-count=5 -race` plus a `GOMAXPROCS=2` pass (that is
  what surfaced the original cross-test interference — a fast laptop hid it).
- Watch for tests that are serial for a *different* reason than the shared
  database. `TestPublicEndpoints` / `TestResolveWebUIURL` in
  `internal/store/global_parameters_test.go` are serial because `public.*`
  parameters are one un-namespaced row-space; with a database per test that is
  no longer shared either, but re-read those comments before deleting them.
