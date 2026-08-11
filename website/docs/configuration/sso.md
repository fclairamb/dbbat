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
| `DBB_OIDC_GROUPS_CLAIM` | ID-token claim carrying directory group membership | `groups` |
| `DBB_OIDC_ROLE_MAPPING` | Binds DBBat roles to directory groups, e.g. `admin=db-admins,viewer=analysts`. Empty leaves role assignment manual | - |

Setting `DBB_OIDC_ISSUER` without both credentials **fails at startup** rather
than shipping a login button that can only ever end on an error page. So does
a malformed `DBB_OIDC_ROLE_MAPPING`: a mapping that cannot be read is an
authorization rule that cannot be read.

What happens when a verified identity has no local account yet is governed by
two settings that apply to every login provider: `DBB_AUTH_AUTO_CREATE_USERS`
(default `true`) and `DBB_AUTH_DEFAULT_ROLE` (default `connector`). A default
role that is not spelled exactly like `admin`, `viewer` or `connector` fails at
startup, for the same reason a malformed mapping does.

Either can be overridden for one provider by appending its name —
`DBB_AUTH_AUTO_CREATE_USERS_SLACK=false` next to an instance-wide `true` lets
this issuer mint accounts while Slack may not. See
[User auto-provisioning](./index.md#user-auto-provisioning-all-login-providers).

## Mapping directory groups to roles

Without a mapping, every auto-provisioned account lands on the default role and
an admin promotes people by hand — and, more to the point, demotes them by
hand, which is the step that gets skipped when someone leaves a team.

`DBB_OIDC_ROLE_MAPPING` makes the directory the source of truth:

```bash
DBB_OIDC_SCOPES="openid email profile groups"
DBB_OIDC_ROLE_MAPPING="admin=db-admins,viewer=analysts"
```

Members of `db-admins` get `admin`, members of `analysts` get `viewer`.

The rules, in full:

- **It is applied on every login, not only at account creation.** Joining the
  group promotes at the next sign-in; leaving it demotes at the next sign-in.
- **It is authoritative only for the roles it names.** In the example above the
  mapping owns `admin` and `viewer`; a `connector` role — or any role an admin
  granted by hand in the UI that the mapping never mentions — is never revoked
  by a login.
- **The default role is the floor.** A user who matches nothing keeps
  `DBB_AUTH_DEFAULT_ROLE` rather than ending up with no roles at all,
  which would be a lockout rather than a demotion.
- **The last admin is never stripped.** If the mapping would remove the `admin`
  role from the only remaining admin, DBBat keeps it and logs a warning —
  the same refusal the users API answers with a 409.
- **Group values are matched exactly, case included**, against whatever the
  claim actually contains. Entra sends group **object ids** (GUIDs), Okta and
  Keycloak send names; DBBat does not try to tell them apart, so paste the
  value your IdP emits.
- **Pairs are separated by commas only.** Group names may contain spaces
  (`admin=Domain Admins`). Repeat a role to union its groups:
  `admin=db-admins,admin=sre`.
- Every resolved change writes a `user.roles_synced` audit entry naming the
  groups that caused it.

Once a mapping is configured, the **Users page says so**: every role it owns is
badged *managed by SSO*, the edit form asks for confirmation before saving a
change to one of them ("this will be undone at the next login"), and a
*SSO sync* column plus a summary on the edit form show the last
`user.roles_synced` entry — when the directory last changed that person's
roles, what it granted or revoked, and the groups it read. Only the role names
reach the browser; group values stay on the server.

:::warning A missing claim reads as "member of nothing"
If the mapping is set but your IdP never emits the claim, every login revokes
the mapped roles. Turn the mapping on only once you have confirmed the claim
arrives — the server logs a warning on each login that carries no groups. The
one exception is an Entra [groups overage](#making-your-idp-emit-groups), where
the issuer says outright that it is withholding the list: that leaves roles
untouched instead.
:::

### Making your IdP emit groups

This is the part nobody's defaults get right. Every provider needs explicit
configuration, and they disagree on the claim name, the value type and whether
a scope is involved. Check yours against its own documentation; below is what
they need as of writing.

**Okta** — on the app: *Sign On → OpenID Connect ID Token → Groups claim type
`Filter`*, claim name `groups`, filter `Matches regex` `.*` (or a tighter
regex). On a custom authorization server, add the claim under *Security → API →
Authorization Servers → Claims* instead, included in the **ID token**. Add
`groups` to `DBB_OIDC_SCOPES`. Values are group **names**.

**Microsoft Entra ID** — *App registrations → your app → Token configuration →
Add groups claim*. Pick **Groups assigned to the application** rather than "All
groups": the "all" options overflow past ~200 memberships into a `_claim_names`
pointer to Microsoft Graph, which DBBat does not follow. Make sure **ID** is
ticked as an emitted token type. No extra scope is needed. Values are group
**object ids** unless you opted into sAMAccountName for on-prem synced groups,
so the mapping reads
`DBB_OIDC_ROLE_MAPPING="admin=1f4e8c02-9b7a-4f61-8a1e-3c2d5e6f7a8b"`.

:::note If you hit the overage anyway
An overflowing token is **not** read as "member of no groups". DBBat detects
the `_claim_names` pointer for whichever claim `DBB_OIDC_GROUPS_CLAIM` names,
treats the membership as *unknown* rather than empty, and leaves every role
exactly as it is — no mapping applied, no floor at `DBB_AUTH_DEFAULT_ROLE`. The
login still succeeds; the server logs a warning naming the user and the claim
on each one. So the failure mode is roles that quietly stop following the
directory, not a mass demotion — fix it by scoping the claim to the groups
assigned to the application.
:::

**Keycloak** — *Client scopes → `<client>-dedicated` → Add mapper → By
configuration → Group Membership*. Token claim name `groups`, **Full group path
off** (leave it on and the values arrive as `/db-admins`), *Add to ID token* on.
Add `groups` to `DBB_OIDC_SCOPES` if you put the mapper on a separate,
optional client scope. Values are group **names**.

**Google Workspace** — Google's OIDC ID tokens carry **no group claim at all**;
membership is only reachable through the Admin SDK, which DBBat does not call.
Role mapping is therefore not available on Google Workspace: manage roles in
DBBat, or front Google with an IdP that does emit groups.

Not sure what your issuer sends? Sign in once with `DBB_OIDC_ROLE_MAPPING`
unset and read the identity's stored provider metadata (the full claim set is
kept on the user identity), or decode an ID token from your IdP's own token
inspector.

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

Each snippet below is the minimum to get people signing in. If you also want
roles to follow directory membership, the env block is not enough on its own —
every IdP needs extra configuration before it emits a groups claim at all: see
[Making your IdP emit groups](#making-your-idp-emit-groups).

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
| "No account is linked to your identity" | Auto-provisioning is off (`DBB_AUTH_AUTO_CREATE_USERS=false`) and no local user matches the email |
| Startup fails with `DBB_OIDC_ROLE_MAPPING is malformed` | A pair without `=`, an empty group, or a role that is not `admin`, `viewer` or `connector` |
| Everyone lost their mapped role at once | The IdP stopped emitting the claim, or it is not the one named by `DBB_OIDC_GROUPS_CLAIM`. The log line "OIDC login carried no group claim" is written on each such login |
| Nobody is promoted, nobody is demoted | The claim arrives but its values do not match the mapping — most often Entra object ids against a mapping written with group names. Matching is exact, case included |
| One user kept `admin` against the mapping | They were the last admin; the role is retained on purpose. Promote someone else first |
