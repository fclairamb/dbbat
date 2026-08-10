# Surface directory-managed roles in the users UI

## Goal

Make it visible, in the admin UI, that a role is owned by the OIDC group
mapping rather than by the person editing the page — and that editing it will
be undone at the user's next login.

## Why

`DBB_OIDC_ROLE_MAPPING` (shipped with the "Map OIDC group claims to dbbat
roles" spec) makes the directory authoritative for the roles it names, applied
on **every** login. The users page has no idea: an admin can tick `admin` on a
user, save it, and watch it silently disappear the next time that user signs
in. The reverse is worse — an admin unticks `admin` to revoke access, the page
says it worked, and the next SSO login puts it straight back because the user
is still in `db-admins`.

Today the only trace is a `user.roles_synced` audit entry nobody is looking at.

No GitHub issue yet — file one when picking this up.

## Implementation

- Expose the mapping through the API: the roles the mapping names, and whether
  it is enabled at all. Cheapest home is the existing settings/parameters
  surface or a field on `GET /api/v1/auth/providers`
  (`internal/api/oauth.go:handleAuthProviders`). The mapping itself is parsed
  by `config.OIDCAuthConfig.ParseRoleMapping` (`internal/config/config.go`);
  return only the role names, never the group values — those are directory
  topology and do not belong in an unauthenticated providers response.
- In `front/`, on the users list and the edit form, badge the mapped roles
  ("managed by SSO") and warn on save when the change touches one.
- Consider showing the last `user.roles_synced` event for the user on their
  detail view — it is the answer to "why did this change?" and the audit rows
  already carry the groups that caused it (`internal/api/oauth_roles.go`,
  `AuditEventOAuthRolesSynced`).
- Related, and cheaper: a users-list column for "last synced from SSO".
