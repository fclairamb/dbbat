# Make the remaining serial subtests independent

## Goal

Remove the last `//nolint:paralleltest` / `//nolint:tparallel` pairs in
`internal/store` by making the subtests they cover self-contained, so every
subtest in the package can call `t.Parallel()`.

## Why

Parallelising the DB-backed suites left 16 parent tests whose subtests still run
serially. None of them is serial because of the database any more — each parent
owns a private database — they are serial because the subtests were written as a
*sequence*: one accumulates counters the next asserts on, one revokes the row the
next expects to be revoked, one empties a table, or the parent inserts rows
between two `t.Run` calls (which a `t.Parallel()` subtest would then observe,
deterministically breaking an "empty" assertion).

That is a test-design smell rather than a constraint. Each of these reads as one
scenario split across `t.Run` boundaries for narration, which is exactly the
shape that resists parallelism and makes a failure hard to localise.

No GitHub issue for this — file one if it is picked up.

## Implementation

The parents still carrying the directives, with the reason recorded on each:

- `internal/store/connections_test.go` — `TestCloseConnection`,
  `TestCloseOrphanedConnections`,
  `TestCloseOrphanedConnectionsReclaimsDeadInstances`,
  `TestReclaimDeadInstanceConnectionsWithoutStartup`,
  `TestCloseOrphanedConnectionsWithSharedInstanceID`,
  `TestIncrementConnectionStats`, `TestIncrementConnectionBytes`
- `internal/store/instances_test.go` — `TestInstanceRegistry`,
  `TestInstanceRegistryTracksRunsSeparately`
- `internal/store/users_test.go` — `TestListUsers`, `TestUpdateUser`,
  `TestEnsureDefaultAdmin`
- `internal/store/api_keys_test.go` — `TestListAPIKeys`
- `internal/store/grants_test.go` — `TestRevokeGrant`
- `internal/store/servers_test.go` — `TestListDatabases`
- `internal/store/user_identities_test.go` — `TestDeleteUserIdentity`

Three distinct fixes, by shape:

1. **Accumulator subtests** (`TestIncrementConnectionStats`,
   `TestIncrementConnectionBytes`): give each subtest its own connection row and
   assert its own totals, or collapse the sequence into one flat test with no
   `t.Run` at all — the narration is worth less than the isolation.
2. **Parent writes between `t.Run` calls** (`TestListUsers`, `TestListDatabases`):
   move the inserts inside the subtest that needs them. A `t.Parallel()` subtest
   body runs *after* the parent function returns, so an "empty list" subtest sees
   the parent's later inserts; this is a guaranteed failure, not a flake.
3. **Store-field mutation** (`TestCloseOrphaned*`, `TestInstanceRegistry*`): the
   subtests call `store.SetInstanceID("")` on the parent's shared `*Store`.
   `Store.instanceID` / `Store.runID` are plain unsynchronised fields
   (`internal/store/store.go`), read by `CreateConnection` and
   `CloseOrphanedConnections`, so sharing one store across parallel subtests is a
   `-race` finding on top of the ordering problem. Give those subtests their own
   store, or take the "unset instance id" cases out into their own top-level test.

Verify the same way this change was verified: `go test ./internal/store/
-count=5 -race` plus a `GOMAXPROCS=2 go test ./internal/... -count=3 -race`
pass, since a single green run proves nothing about concurrency.
