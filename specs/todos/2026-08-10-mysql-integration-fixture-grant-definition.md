# Repair the MySQL integration fixture's grant creation

No GitHub issue yet — file one when picking this up.

## Goal

Make `make test-integration-mysql` reach its assertions again. The suite
currently dies at setup with `a grant must reference a grant definition`, so
none of the MySQL proxy's end-to-end behaviour is actually being verified.

## Why

Grants became instances of grant definitions: `store.CreateGrant` now requires
`GrantDefinitionID`, and an inline `Definition: &store.GrantDefinition{…}` on
the grant is no longer enough (it answers `ErrGrantDefinitionRequired`). Three
integration fixtures were written before that change.

Two of the three have already been repaired — PostgreSQL in
`32efdb8` and MongoDB + Oracle while implementing the MCP phase-2 spec — and
each repair uncovered real coverage that had silently stopped running. MySQL is
the last one, and it is not a small suite to have dark.

## Implementation

`internal/proxy/mysql/integration_test.go` has two call sites, both using the
pre-change shape:

- line ~224, in the fixture setup
- line ~491, the read-only grant a test swaps in

Copy the helper the other three suites now carry (identical in
`internal/proxy/postgresql/integration_test.go`,
`internal/proxy/mongodb/integration_test.go` and
`internal/proxy/oracle/integration_test.go`):

```go
func createGrantWithControls(
    ctx context.Context, t *testing.T, dataStore *store.Store,
    userUID, databaseUID uuid.UUID, controls []string,
) (*store.Grant, error)
```

It creates the definition first (with `CreatedBy` set — the foreign key
requires it) and issues the grant against `def.UID`. A unique slug per call
matters: a suite that seeds twice in one store otherwise trips
`grant definition with this slug already exists`.

Then run `make test-integration-mysql` and fix whatever the newly-running tests
turn up — the PostgreSQL repair exposed a polling race the moment its tests
started executing again, so expect the same kind of second-order fallout rather
than an immediately green suite.

**While you are there**, consider hoisting the helper into a small shared
test-support package instead of a fourth copy; four identical definitions of
the same fixture is how the next schema change breaks four suites again.
