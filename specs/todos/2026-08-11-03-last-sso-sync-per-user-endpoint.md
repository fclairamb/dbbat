# Make "last synced from SSO" exact on a busy instance

## Goal

Give the users page a per-user answer for "when did the directory last change
this person's roles", instead of one derived from a bounded slice of the audit
log.

## Why

"Surface directory-managed roles in the users UI" shipped the *SSO sync* column
and the last-sync summary on the edit form by fetching the newest 200
`user.roles_synced` audit entries in one request
(`useLastRoleSyncs`, `front/src/api/queries.ts`) and keeping the first entry
seen per user. That is right on a small instance and cheap everywhere, but it
degrades quietly: on a deployment where hundreds of people sign in through SSO
and change groups often, a user whose last sync fell out of the 200-entry
window is rendered as **Never synced** — the same thing the page shows for a
user the directory has genuinely never touched. Raising the limit moves the
cliff, it does not remove it, and `DBB_QUERY_STORAGE_RETENTION`-style pruning of
old audit rows would eventually do the same to everyone.

No GitHub issue yet — file one when picking this up.

## Implementation

Two candidate shapes, cheapest first:

- **A projection on the user row.** Add `last_roles_synced_at` (and possibly
  the audit entry's UID) to `users`, written in the same transaction as the
  audit entry in `Server.auditRoleSync` (`internal/api/oauth_roles.go`). The
  list column then needs no extra request at all, and "Never" becomes truthful
  because it is a column, not a window. Costs a migration
  (`internal/migrations/sql/`), and the column is a cache of the audit log —
  it must never become the *authority* for what happened, only a pointer to it.
- **A narrow endpoint.** `GET /api/v1/users/role-syncs` returning the latest
  `user.roles_synced` per user (`DISTINCT ON (user_id) … ORDER BY user_id,
  uid DESC`), admin-or-viewer like the audit list it reads. No migration, one
  request per page load, exact regardless of volume.

Either way, keep the edit form's summary reading the full audit entry — the
groups it carries are the answer to "why did this change?", and neither shape
above should drop them. Update `front/src/components/shared/SsoRoleSync.tsx`
and the users page accordingly, and extend
`front/e2e/users.spec.ts` (test mode already seeds a role sync for the `viewer`
user in `provisionTestData`).
