# A server can never be renamed

## Goal

Let an admin change a server's `name` through `PUT /api/v1/servers/:uid` (and the
admin UI form), with the same validation the create path applies.

## Why

`name` is settable at creation and immutable forever after: it is absent from
`UpdateDatabaseRequest` (`internal/api/servers.go:61`), from `store.ServerUpdate`
(`internal/store/models.go:358`), and therefore from the UI form. Every other
field on the row — host, port, credentials, protocol, tunnel, approvers — can be
corrected in place; the one identifier users actually type cannot.

That matters because the name is not cosmetic: it is the lookup key clients send
as the PostgreSQL database, the MySQL schema, the MongoDB database and the Oracle
SERVICE_NAME (`store.GetServerByName`, called from all five proxies plus
`internal/mcp/exec.go`). A badly chosen name is a badly chosen connection string
for everyone, forever.

Concretely, on 2026-08-13 the production instance (`dbbat.tools.stonal.io`) had
eight Oracle servers named `abyla_abymutualise (Admin)`, `abyla_abypocs (R/O)`
and so on — spaces, parentheses and a slash inside a value clients must send as
an Oracle SERVICE_NAME. The only way to fix them was a hand-written `UPDATE
servers SET name = ...` against the production storage database, bypassing the
API, its validation and its `database.updated` audit entry. Renaming them to
`abyla_<instance>_admin` / `_ro` (matching the `prod_*_ro` / `_rw` rows) took a
direct SQL write it should never have taken.

The alternative available today — delete and re-create — is worse: it drops the
grants, the connection history and the query chains hanging off `database_id`.

## Implementation

- Add `Name *string` to `UpdateDatabaseRequest` (`internal/api/servers.go`) and to
  `store.ServerUpdate` (`internal/store/models.go`), wired through
  `handleUpdateDatabase` and `Store.UpdateServer` like the other pointers.
- Validate exactly as the create path does — reuse whatever `handleCreateDatabase`
  applies to `name`, and consider tightening it there at the same time: a name
  containing whitespace, `(`, `)` or `/` is unusable as an Oracle SERVICE_NAME and
  awkward in every other protocol's connection string. Rejecting those at the API
  is what stops this recurring.
- `servers_name_key` is a global unique constraint that also covers soft-deleted
  rows, so map a collision to a 409 rather than a 500. Note the production row
  `prod_datalake (Admin)` (uid `cb28dc80-…`, `deleted_at` 2026-08-06) still holds
  its name and would block reuse.
- Surface the field in the admin UI's server edit form, with a warning that the
  name is the connection target: renaming breaks every saved connection string
  and every client config using the old one. Live sessions already authenticated
  are unaffected; new connects must use the new name.
- Document the rename in `internal/api/openapi.yml` (`UpdateDatabaseRequest`) and
  make sure it lands in the `database.updated` audit details via
  `redactUpdateForAudit`.
