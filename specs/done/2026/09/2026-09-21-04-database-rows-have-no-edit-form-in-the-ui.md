# Database rows have no edit form in the UI

Issue: https://github.com/fclairamb/dbbat/issues/393

## Goal

Let an admin correct a database row (host, port, database name, credentials,
SSL mode, tunnel, listability) from the Servers page, the way an SSH or
Kubernetes row already can, with a blast-radius warning on the fields that
change *where* the row points.

## Why

`PUT /api/v1/servers/:uid` already accepts every field — host, port,
`database_name`, username, password, `ssl_mode`, `via_uid`, `listable`, even
`protocol` — and `useUpdateDatabase` in `front/src/api/queries.ts` already
wraps it. The UI simply never calls it for a database row: the actions column
in `front/src/routes/_authenticated/servers/index.tsx` offers *test*, *rename*,
*approvers* and *delete*, and two code comments say it outright ("database rows
have no general edit form in this UI"). Tunnel rows, by contrast, get
`EditSSHServerForm` with name, description, host, port, username, password and
the kind-specific material.

So the restriction protects nothing. The concern it was meant to address — an
edit turning a row into a *different* server while every grant bound to its
`database_id` follows along — is real, but an admin with an API key can do it
today, and the UI refusing to is only friction for the honest path. The
workaround, delete and re-create, is the worse outcome: it drops the grants,
the connection history and the query chains hanging off the row (the same
argument `specs/done/2026/08/2026-08-13-23-servers-cannot-be-renamed.md` made
for `name`).

The project already accepted exactly this trade-off once: server-group
membership is live, widens running grants, and the answer was a warning at the
point of edit rather than immutability. Same treatment here.

## Implementation

- **Edit dialog for database rows**, next to the rename one, modelled on
  `EditSSHServerForm` (keyed on the uid so it re-seeds per row). Fields by
  protocol, mirroring the create dialog: description, host, port,
  `database_name` (or `oracle_service_name` on Oracle, plus
  `mongo_auth_source` on MongoDB), username, password (blank = keep, like the
  SSH secrets), `ssl_mode` (not on Oracle), `listable`, and the tunnel
  (`via_uid` / `clear_via_uid`). Keep `name` in `RenameServerDialog`: it has its
  own warning because it changes what clients type.
- **Leave `protocol` out of the form.** Changing the protocol of a row that has
  grants and history is the one edit that genuinely makes it another server.
  Consider having the API refuse a protocol change on a row with any grant or
  connection (409), so the invariant is enforced where it can be, not hidden in
  the UI.
- **Blast-radius warning** when host, port or the database/service name
  differ from the stored value: count the active grants and the server groups
  that reference the row (both are one query each) and say that they will
  reach the new target on next connect. Sessions already open stay on the old
  upstream; that is the rename's semantics too and needs no new mechanism.
- **Offer "test connection after saving"** as a checkbox: the API's
  `test_connection` flag already returns the result inline on the PUT.
- **Close the audit gap the concern actually points at.** `connection.opened`
  (`internal/store/connection_audit.go`) records `database_id` and nothing
  about the target, so once the target is editable the ledger no longer says
  where a session went. Add host, port and the database/service name to the
  entry's details — they are immutable for the *session* even if not for the
  row. Then a later edit changes nothing about the evidence, which is the
  property the immutability was standing in for.
- Only send fields that changed (the tunnel form's `renamed` trick), so the
  `database.updated` audit entry lists real edits. `redactUpdateForAudit`
  already covers the secrets.
- E2E: edit host on a seeded row, see the warning, save, re-read the row; and
  a rename-free edit must leave `name` untouched in the audit details.

## Implementation Plan

1. **Store — `GetServerReferences`** (`internal/store/servers.go`). One struct,
   two counts: the *live* grants that would reach the new target (anchor
   `database_id` **or** a `server_group_uid` whose group holds this server,
   under the auth path's own `applyGrantLiveness`) and the server groups the
   row belongs to. One query each, mirroring `GetServerGroupBlastRadius`.
   Unit test in `internal/store`.

2. **API — `GET /servers/{uid}/references`** (admin), returning
   `{active_grants, server_groups}`. Registered next to `/servers/{uid}/test`,
   documented in `openapi.yml` (the parity test fails otherwise), handler test
   in `internal/api`.

3. **API — refuse a protocol change on a row with history.** `PUT
   /servers/{uid}` accepts `protocol` today with no guard at all. Add one in
   `handleUpdateDatabase`: a request whose `protocol` differs from the stored
   one on a row carrying any grant (revoked included) or any connection is a
   **409**. Same counts, reused from step 1's store helper plus a connection
   count. Handler tests for both the refusal and the still-allowed change on a
   pristine row.

4. **Audit — target on `connection.opened`.** `connectionAuditDetails` gains
   `host`, `port` and `database` (the Oracle service name on Oracle,
   `database_name` elsewhere), read from the `servers` row inside
   `recordConnectionOpened` so no proxy call site has to be touched and the
   property holds unconditionally. Never fatal: a lookup failure leaves the
   fields empty rather than dropping the entry. Store test.

5. **Frontend API layer.** Regenerate `front/src/api/schema.ts`, add
   `useServerReferences(uid, enabled)`, and let `useUpdateDatabase` hand the
   PUT's inline `connection_test` back to its `onSuccess`.

6. **Frontend — `EditDatabaseDialog` / `EditDatabaseForm`** in
   `front/src/routes/_authenticated/servers/index.tsx`, keyed on the row uid,
   opened from a new pencil-adjacent action on the database actions column.
   Fields: description, host, port, `database_name` /
   `oracle_service_name` (+ `mongo_auth_source` on MongoDB), username, password
   (blank = keep), `ssl_mode` (not on Oracle), `listable`, tunnel (`via_uid` /
   `clear_via_uid`). No `protocol`. Blast-radius `Alert` whenever host, port or
   the database/service name differs from the stored value, fed by
   `useServerReferences`. A "test connection after saving" checkbox setting
   `test_connection`. Submit diffs against the seeded row and sends only what
   changed.

7. **E2E** — extend `front/e2e/servers.spec.ts`: open the edit dialog on a
   seeded database row, change the host, assert the blast-radius warning, save,
   reopen and confirm the new host; then assert the `database.updated` audit
   entry's `updated_fields` carries `host` and **no** `name`.

8. **QA** — `make lint`, `go test ./internal/api/... ./internal/store/...`,
   `make test`, `bun run lint` + `make build-front`, and the new Playwright
   spec.
