# Link an OAuth/OIDC identity to the current account from a logged-in session

## Goal

Let a signed-in user (an admin in particular) attach a Slack or OIDC identity to
their own account explicitly: "Link my SSO identity" in the profile page starts
the provider's authorization flow with a state row bound to the session's user,
and the callback links the returned identity onto that user instead of resolving
it through `findOrCreateOAuthUser`.

## Why

Since `2026-10-30-08-oauth-link-requires-verified-email`, an unlinked identity is
linked by email match only when the provider asserts `email_verified: true`, and
never onto an account holding `admin`. That closes an account takeover, but
leaves an admin (and anyone behind an issuer that omits `email_verified`) with no
way to use SSO on their existing account: their first SSO login lands on a new
auto-created account, or is refused. A linked Slack identity is also what lets
someone click Approve/Deny in Slack (`internal/api/slack_interactions.go`), so an
admin approver currently cannot get one without an operator inserting the
`user_identities` row by hand.

## Implementation

- `POST /api/v1/auth/{provider}/link` (authenticated, session only, not an API
  key): creates the OAuth state row with a `link_user_id` column (new migration)
  and returns the authorize URL.
- The callback, when the state row carries `link_user_id`, refuses an identity
  already linked to another user (409), otherwise calls `linkIdentity` onto
  `link_user_id`, writes `user.identity_linked` with `method: "session"`, and
  redirects back to the profile page.
- `DELETE /api/v1/users/me/identities/{provider}` to unlink, audited.
- Front: profile page lists linked identities with link/unlink buttons
  (`data-testid` on each).
- Key files: `internal/api/oauth.go`, `internal/store/oauth_state*.go`,
  `internal/migrations/sql/`, `internal/api/openapi.yml`, `front/src/`.
