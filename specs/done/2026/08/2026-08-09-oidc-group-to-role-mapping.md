# Map OIDC group claims to dbbat roles

## Goal

Let an operator say "members of the `db-admins` IdP group get the `admin` role,
everyone else gets `connector`", so role assignment follows the directory
instead of being hand-maintained per user in dbbat.

## Why

The generic OIDC provider (`internal/auth/oidc`) now authenticates users
against any issuer, but every auto-provisioned account lands on the single
static default role. In an org of any size that means an admin manually
promoting each new engineer, and — worse — manually demoting them, which is
exactly the step that gets skipped when someone leaves a team. The `groups`
claim is already requestable (`DBB_OIDC_SCOPES`) and Okta, Entra and Keycloak
all emit one; the ID token is verified, so the claim is trustworthy input.

This is the cheap 80% of "directory sync" without taking on SCIM.

No GitHub issue yet — file one when picking this up.

## Implementation

- `DBB_OIDC_GROUPS_CLAIM` (default `groups`) and `DBB_OIDC_ROLE_MAPPING`
  (`admin=db-admins,viewer=analysts`) in `OIDCAuthConfig`
  (`internal/config/config.go`). Entra sends group **object ids**, not names —
  document that, and match on the raw claim value either way.
- Parse the claim in `ExchangeCodeWithVerifier`
  (`internal/auth/oidc/provider.go`) and carry it on `auth.OAuthUser`. That
  needs a home: add a `Groups []string` field to `auth.OAuthUser`
  (`internal/auth/oauth.go`) rather than making callers re-parse `RawData`.
  Handle both the array and the single-string encodings issuers use.
- Apply on **every** login, not just creation, in `findOrCreateOAuthUser`
  (`internal/api/oauth.go`) — the demotion case is the whole point. Decide
  explicitly what happens to roles granted manually in the UI: safest is to
  make mapping authoritative only for the roles named in the mapping, and to
  write an audit entry whenever the resolved set changes.
- Never let a mapping empty a user's roles into a lockout: keep the default
  role as the floor, and refuse to strip the last `admin` the same way user
  management already does.
- Tests: extend `internal/auth/oidc/provider_test.go` (the httptest issuer
  already signs arbitrary claims — add `groups` to them) and cover the
  promote/demote transitions in `internal/api/oauth_test.go`.
- Docs: `website/docs/configuration/sso.md` per-IdP snippets need the extra
  scope/claim configuration each provider requires to emit groups at all.
