# Generic OIDC SSO provider

## Goal

Let any org sign in to dbbat with its own identity provider — Google
Workspace, Okta, Microsoft Entra, Keycloak, Authentik — by adding a generic,
configurable-issuer OIDC provider alongside the existing Slack one.

## Why

Login today is Slack-or-local-password: `internal/auth/slack/provider.go`
hardcodes Slack's OIDC endpoints. Any organization not running Slack (or not
willing to wire auth through it) hits a wall on day one — this is likely
dbbat's single biggest silent adoption blocker, and the competitive landscape
doc lists "enterprise identity depth" as an honest gap. The
`auth.OAuthProvider` abstraction (`internal/auth/oauth.go`) is already
provider-agnostic, and `findOrCreateOAuthUser`
(`internal/api/oauth.go:292`) doesn't care which provider vouched for the
user, so this is a well-contained addition. SCIM/directory sync can wait;
OIDC can't.

No GitHub issue yet — file one when picking this up.

## Implementation

### New provider package

- `internal/auth/oidc/provider.go` implementing `auth.OAuthProvider`
  (`Name`, `AuthorizeURL`, `ExchangeCode`).
- Use `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`: discovery from
  `<issuer>/.well-known/openid-configuration`, code exchange, and **ID-token
  signature verification** (the Slack provider trusts Slack's userInfo
  endpoint; a generic provider must verify the JWT against the issuer's
  JWKS).
- Map standard claims (`sub`, `email`, `name`, `picture`) to
  `auth.OAuthUser`; keep the full claim set in `RawData`.
- Add PKCE (S256) — cheap with `x/oauth2`, and some IdPs require it.

### Config

New `OIDCAuthConfig` in `internal/config/config.go`, next to
`SlackAuthConfig` (~line 172):

- `DBB_OIDC_ISSUER` — enables the provider when set
- `DBB_OIDC_CLIENT_ID`, `DBB_OIDC_CLIENT_SECRET`
- `DBB_OIDC_SCOPES` — default `openid email profile`
- `DBB_OIDC_DISPLAY_NAME` — login-button label, default "SSO"
- `DBB_OIDC_EMAIL_DOMAINS` — optional comma-separated allowlist; the generic
  equivalent of the Slack provider's team-ID gating. Reject the login when
  the verified email's domain isn't listed.

### Wiring

- `internal/api/server.go:107-109` builds the `oauthProviders` map — add an
  `"oidc"` entry when configured. The existing authorize/callback handlers
  and OAuth state store (`internal/store/oauth_states.go`) are
  provider-keyed already.
- Frontend: the login page currently knows about Slack. Find where it
  discovers available providers and render a generic SSO button using
  `DBB_OIDC_DISPLAY_NAME` (see `front/`).
- Docs: environment-variable table in `CLAUDE.md`/README + a website page
  with per-IdP setup snippets (Google, Okta, Entra, Keycloak) — those four
  snippets are most of the adoption value.

### Tests

- Mirror `internal/auth/slack/provider_test.go` with an httptest OIDC issuer
  (static JWKS, signed ID tokens): happy path, bad signature, wrong
  audience/issuer, domain-allowlist rejection.
- `internal/api/oauth_test.go` already has a mock-provider harness — extend
  for the second registered provider.
