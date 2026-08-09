---
sidebar_position: 3
sidebar_label: Single sign-on (OIDC)
title: Single sign-on (OIDC)
description: Sign in to DBBat with your own identity provider — Google Workspace, Okta, Microsoft Entra, Keycloak or anything else that speaks OpenID Connect.
---

# Single sign-on (OIDC)

DBBat ships a **generic OpenID Connect provider**: point it at your issuer and
your engineers sign in with the identity they already have. It works with
anything that publishes an OIDC discovery document — Google Workspace, Okta,
Microsoft Entra ID, Keycloak, Authentik, Auth0, Ping, Dex.

It sits alongside the [Slack login](./index.md#slack-oauth-optional) and the
local password form: enable one, two, or all three. The login page renders one
button per enabled provider.

## What DBBat verifies

Every sign-in is carried by an **ID token that DBBat verifies itself** against
the issuer's JWKS — signature, `iss`, `aud` and expiry — before any local user
is looked up or created. The authorization code flow always carries a **PKCE
(S256)** challenge, and the code verifier never leaves the server: it is stored
with the CSRF state row, so a callback that lands on a different replica still
completes.

If your IdP sends `email_verified: false`, the login is refused. If you
configure `DBB_OIDC_EMAIL_DOMAINS`, the check runs against that **verified**
email claim, never against anything the browser supplied.

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_OIDC_ISSUER` | Issuer URL. **Setting it enables the provider.** Discovery is done against `<issuer>/.well-known/openid-configuration` | - |
| `DBB_OIDC_CLIENT_ID` | Client ID registered with the IdP. Required once the issuer is set | - |
| `DBB_OIDC_CLIENT_SECRET` | Client secret. Required once the issuer is set | - |
| `DBB_OIDC_SCOPES` | Space- or comma-separated scopes. `openid` is always added | `openid email profile` |
| `DBB_OIDC_DISPLAY_NAME` | Login-button label, e.g. `Acme SSO` | `SSO` |
| `DBB_OIDC_EMAIL_DOMAINS` | Optional comma-separated allowlist of email domains. Empty = any domain the IdP vouches for | - |

Setting `DBB_OIDC_ISSUER` without both credentials **fails at startup** rather
than shipping a login button that can only ever end on an error page.

User auto-provisioning is shared with the Slack provider:
`DBB_SLACK_AUTH_AUTO_CREATE_USERS` (default `true`) and
`DBB_SLACK_AUTH_DEFAULT_ROLE` (default `connector`) govern what happens when a
verified identity has no local account yet.

:::warning Set the allowlist on a multi-tenant issuer
Google's `https://accounts.google.com` and Entra's `/common` endpoint will
happily authenticate **any** Google or Microsoft account on the internet. On
those issuers `DBB_OIDC_EMAIL_DOMAINS` (or a tenant-scoped issuer URL) is what
stops a stranger from auto-provisioning themselves an account.
:::

## The redirect URI

Whatever the IdP calls it — redirect URI, callback URL, reply URL — register
exactly:

```
https://dbbat.example.com/api/v1/auth/oidc/callback
```

Substitute your externally reachable host. DBBat derives the scheme from the
request, honouring `X-Forwarded-Proto`, so terminate TLS at your ingress and
make sure that header is set.

## Per-provider setup

### Google Workspace

1. In the [Google Cloud console](https://console.cloud.google.com/apis/credentials),
   create an **OAuth client ID** of type *Web application*.
2. Add `https://dbbat.example.com/api/v1/auth/oidc/callback` as an authorized
   redirect URI.
3. Configure the OAuth consent screen as **Internal** so only your Workspace
   can use it.

```bash
DBB_OIDC_ISSUER=https://accounts.google.com
DBB_OIDC_CLIENT_ID=1234567890-abcdef.apps.googleusercontent.com
DBB_OIDC_CLIENT_SECRET=GOCSPX-...
DBB_OIDC_DISPLAY_NAME=Google
DBB_OIDC_EMAIL_DOMAINS=example.com
```

Google's issuer is shared by every Google account, so keep the allowlist —
"Internal" consent alone is not a token-level restriction.

### Okta

1. **Applications → Create App Integration → OIDC / Web Application**.
2. Sign-in redirect URI: `https://dbbat.example.com/api/v1/auth/oidc/callback`.
3. Assign the app to the groups that should reach DBBat.

```bash
DBB_OIDC_ISSUER=https://acme.okta.com
DBB_OIDC_CLIENT_ID=0oa1abcdefGHIJKL2m3n4
DBB_OIDC_CLIENT_SECRET=...
DBB_OIDC_DISPLAY_NAME=Okta
```

Using an [Okta authorization server](https://developer.okta.com/docs/concepts/auth-servers/)
other than the org one? The issuer becomes
`https://acme.okta.com/oauth2/<server-id>`. Access is already scoped by the
app assignment, so the email allowlist is optional here.

### Microsoft Entra ID

1. **Microsoft Entra ID → App registrations → New registration**.
2. Platform *Web*, redirect URI
   `https://dbbat.example.com/api/v1/auth/oidc/callback`.
3. **Certificates & secrets → New client secret**.

```bash
DBB_OIDC_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
DBB_OIDC_CLIENT_ID=<application-client-id>
DBB_OIDC_CLIENT_SECRET=...
DBB_OIDC_DISPLAY_NAME=Microsoft
```

Use your **tenant GUID** in the issuer, not `common` or `organizations`: the
multi-tenant endpoints issue tokens for accounts outside your directory, and
their `iss` claim is tenant-specific anyway, which breaks issuer validation.
Entra only puts `email` in the ID token when the account has one — add the
`email` optional claim to the token configuration if sign-ins arrive without
it.

### Keycloak

1. In your realm: **Clients → Create client**, type *OpenID Connect*.
2. Turn **Client authentication** on (a confidential client), enable the
   *Standard flow*.
3. Valid redirect URI:
   `https://dbbat.example.com/api/v1/auth/oidc/callback`.
4. Copy the secret from the **Credentials** tab.

```bash
DBB_OIDC_ISSUER=https://keycloak.example.com/realms/acme
DBB_OIDC_CLIENT_ID=dbbat
DBB_OIDC_CLIENT_SECRET=...
DBB_OIDC_DISPLAY_NAME=Acme SSO
```

The realm URL *is* the issuer — no `/protocol/openid-connect/auth` suffix.
Keycloak realms are already tenant-scoped, so the email allowlist is optional.

## Troubleshooting

| Symptom | Cause |
|---------|-------|
| Startup fails with `DBB_OIDC_ISSUER requires DBB_OIDC_CLIENT_ID and DBB_OIDC_CLIENT_SECRET` | Issuer set, credentials missing |
| No SSO button on the login page | `DBB_OIDC_ISSUER` unset, or the process never restarted |
| `oidc discovery` error in the logs | Issuer URL wrong, or the IdP is unreachable from DBBat. Discovery is lazy — the first login attempt is what surfaces it |
| Login bounces back with "Failed to complete single sign-on" | Check the server log for the exchange error: redirect-URI mismatch, wrong client secret, or ID-token verification failure |
| `id token verification` failure | `aud` or `iss` mismatch — usually a multi-tenant issuer URL, or credentials from a different app registration |
| "Your workspace or email domain is not authorized" | The verified email's domain is outside `DBB_OIDC_EMAIL_DOMAINS` |
| "No account is linked to your identity" | Auto-provisioning is off (`DBB_SLACK_AUTH_AUTO_CREATE_USERS=false`) and no local user matches the email |
