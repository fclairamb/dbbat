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
