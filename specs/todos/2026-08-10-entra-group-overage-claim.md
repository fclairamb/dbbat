# Handle the Entra groups overage claim (or detect and refuse it)

## Goal

Stop a Microsoft Entra ID user who belongs to many groups from silently
appearing to belong to none, which under `DBB_OIDC_ROLE_MAPPING` revokes every
mapped role.

## Why

Entra does not put an unbounded group list in an ID token. Past roughly 200
memberships it drops the `groups` claim entirely and substitutes
`_claim_names` / `_claim_sources`, a pointer to a Microsoft Graph endpoint the
caller is expected to call. dbbat reads the claim and nothing else
(`extractGroups` in `internal/auth/oidc/provider.go`), so an overage token
looks exactly like "this user is in no groups": the role mapping revokes the
mapped roles, floors the user at the default role, and the only clue is the
"OIDC login carried no group claim" warning.

The documentation currently steers operators around it (`Groups assigned to
the application` instead of `All groups`, in
`website/docs/configuration/sso.md`), which works but depends on the reader
following it. A large tenant that ticks the wrong box gets a silent mass
demotion.

No GitHub issue yet — file one when picking this up.

## Implementation

Two options, in increasing cost:

1. **Detect and fail closed.** In `extractGroups`, notice `_claim_names`
   carrying an entry for the configured groups claim and surface it as a
   distinct signal on `auth.OAuthUser` (or an error). `syncOAuthRoles`
   (`internal/api/oauth_roles.go`) then leaves roles untouched and logs loudly,
   rather than treating the absence as "member of nothing". This is small,
   has no new network dependency, and turns a silent demotion into a visible
   misconfiguration. Probably the right first step.
2. **Follow the pointer.** Call the Graph `getMemberObjects` endpoint with the
   access token from the exchange. That needs a Graph scope, a second outbound
   dependency in the login path, and a timeout budget on an already
   user-blocking flow. Only worth it if a real deployment asks.

Tests belong in `internal/auth/oidc/provider_test.go` (the httptest issuer
signs arbitrary claims, so an overage token is a two-line fixture) and in
`internal/api/oauth_roles_test.go` for the "roles untouched" outcome.
