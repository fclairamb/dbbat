# Server names: unique among live servers only, so a deleted name can be reused

## Goal

Deleting a server frees its name. Creating or renaming a server to the name of a
soft-deleted one succeeds; a name held by a live server is still a 409
`DUPLICATE_NAME`.

## Why

`servers_name_key` was a plain `UNIQUE (name)` constraint, so it also covered
soft-deleted rows. A delete is a soft delete (`deleted_at`), and there is no
restore and no purge, so **a deleted server's name was taken forever**. The
deleted row is invisible to `GET /servers`, which made the 409 look unexplained.

Seen in production on 2026-11-05: a provisioning script declared the PostgreSQL
databases of a nonprod cluster as `<stack>_<service>_rw` (`dev_datalake_rw`,
and so on). Those names had been used and deleted before, so every
`POST /servers` answered `409 a server with this name already exists`. The only
ways out were a hand-written `UPDATE` against the storage database, or a naming
convention that drifts from every other cluster.

`users` had the same problem and fixed it with `users_username_active_uq`
(`20260511000000_users_username_partial_unique`). This change applies the same
fix to servers.

## Implementation

- Migration `20261105000000_servers_name_partial_unique`:
  - Drops `servers_name_key`.
  - Creates `servers_name_active_uq ON servers(name) WHERE deleted_at IS NULL`.
  - The down migration restores the global constraint. It fails if a live row
    reuses a deleted row's name, which is expected: one of the two has to be
    renamed first.
  - `idx_servers_name`, the plain lookup index, stays.
- `Store.CreateServer` and `Store.UpdateServer` map a violation of
  `servers_name_active_uq` to `ErrServerNameConflict`. The API still answers 409.
- Several rows may now share a name, but at most one of them is live, so no
  lookup is ambiguous. Every lookup by name (`GetServerByName`, which all five
  proxies use to resolve their target) goes through the bun `soft_delete` model,
  which already excludes deleted rows. No raw SQL selects servers by name.
- Docs updated: the comments in `internal/api/servers.go` and the
  `UpdateDatabaseRequest.name` description in `openapi.yml` and the generated
  `front/src/api/schema.ts`.
- Tests:
  - `TestUpdateServer_RenameConflict` and `TestUpdateServerRenameConflictIsA409`
    now assert that a rename onto a deleted server's name **succeeds**, and that
    a rename onto a live server's name is still a conflict.
  - New `TestCreateServer_ReusesDeletedName` covers delete, then re-create under
    the same name, then a third create that conflicts. It also checks that
    `GetServerByName` returns the live row, not the deleted one.
