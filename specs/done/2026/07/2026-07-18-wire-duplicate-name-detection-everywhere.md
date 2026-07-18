# Wire duplicate-name conflict detection across all create endpoints

## Goal
Return a clean `409 CONFLICT` / `DUPLICATE_NAME` (with a human-readable message)
whenever a create hits a unique-name constraint, on every resource — not just
servers.

## Why
Creating a server with an already-taken name used to return a generic
`500 INTERNAL_ERROR` ("An internal error occurred"). Fixed for servers on
2026-07-18 by detecting the `servers_name_key` unique violation in
`store.CreateServer` and mapping `store.ErrServerNameConflict` to a 409 in the
API handler (see `internal/store/errors.go` `isUniqueViolation`,
`internal/store/servers.go`, `internal/api/servers.go`).

Other resources still lack this. Notably:
- `store.ErrGrantDefinitionDuplicate` is **defined but never returned** — grant
  definition creation likely 500s on a duplicate name today.
- `internal/api/errors.go` `ErrCodeDuplicateName` was defined but unused before
  this fix; audit other create paths (users, API keys, grant definitions) for
  the same missing detection.

## Implementation
- Reuse the `isUniqueViolation(err, constraint)` helper in
  `internal/store/errors.go` (detects SQLSTATE 23505, optional constraint-name
  scope via PG error field `'n'`).
- In each store Create* that can violate a unique-name constraint, map the
  driver error to the resource's typed sentinel (e.g. actually return
  `ErrGrantDefinitionDuplicate`).
- In the matching API handler, `errors.Is` the sentinel and
  `writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())`.
- Document the `409` response in `internal/api/openapi.yml` for each endpoint
  (a reusable `Conflict` response component now exists).
- Add store-level unit tests mirroring `TestCreateServer_DuplicateName`.

No originating GitHub issue yet — file one if this is picked up.

## Implementation Plan

Audit of every `Create*` store function with a unique-name constraint:

| Resource | Store fn | Unique index | Sentinel | Handler |
|----------|----------|--------------|----------|---------|
| Servers (done) | `CreateServer` | `servers_name_key` | `ErrServerNameConflict` | `internal/api/servers.go` |
| Grant definitions | `CreateGrantDefinition` | `grant_definitions_active_name_uniq` | `ErrGrantDefinitionDuplicate` (defined, never returned) | `internal/api/grant_definitions.go` |
| Users | `CreateUser` | `users_username_active_uq` | `ErrUserNameConflict` (new) | `internal/api/users.go` |
| API keys | `CreateAPIKey` | only `unique_key_prefix` (random prefix, not a user-facing name) | none needed | n/a |

Steps:
1. `internal/store/grant_definitions.go`: in `CreateGrantDefinition`, map
   `isUniqueViolation(err, "grant_definitions_active_name_uniq")` → `ErrGrantDefinitionDuplicate`.
2. `internal/api/grant_definitions.go`: `errors.Is(err, store.ErrGrantDefinitionDuplicate)` →
   `writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())`.
3. `internal/store/errors.go`: add `ErrUserNameConflict`.
4. `internal/store/users.go`: in `CreateUser`, map
   `isUniqueViolation(err, "users_username_active_uq")` → `ErrUserNameConflict`.
5. `internal/api/users.go`: `errors.Is(err, store.ErrUserNameConflict)` → 409 `DUPLICATE_NAME`.
6. `internal/api/openapi.yml`: add `'409': $ref '#/components/responses/Conflict'` to the
   `/users` POST and `/grant-definitions` POST responses.
7. Tests: `TestCreateGrantDefinition_DuplicateName` and `TestCreateUser_DuplicateName`
   mirroring `TestCreateServer_DuplicateName`.
8. API keys: no unique-name constraint → nothing to wire; documented in the audit table above.
