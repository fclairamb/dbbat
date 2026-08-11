# Per-provider override for OAuth auto-provisioning

## Goal

Let a deployment say "the corporate OIDC issuer may auto-provision accounts,
Slack may not" (or the reverse), instead of the single instance-wide
`DBB_AUTH_AUTO_CREATE_USERS` / `DBB_AUTH_DEFAULT_ROLE` pair.

## Why

The 2026-08-09 spec moved those two settings off `SlackAuthConfig` onto the
provider-agnostic `config.OAuthUsersConfig` and deliberately stopped there —
one knob for every provider. That is right for the common case, and it is the
whole point of the rename.

It is not enough for a deployment running two providers with different trust
levels: a tightly-gated Entra tenant where auto-provisioning is exactly what you
want, alongside a Slack workspace that contains contractors and should only let
in accounts an admin created by hand. Today the stricter provider's policy wins
for both, so the looser one is effectively unusable.

No GitHub issue yet — file one when picking this up. (The parent spec's issue
was never filed either.)

## Implementation

- The read points already exist and are the only ones: `oauthAutoCreateUsers()`
  and `oauthDefaultRole()` in `internal/api/oauth_roles.go`. Both would take the
  provider name — every caller already has it (`findOrCreateOAuthUser` in
  `internal/api/oauth.go`, `oauthRolesForNewUser`, `syncOAuthRoles`).
- Shape: `DBB_AUTH_AUTO_CREATE_USERS_<PROVIDER>` (e.g. `..._OIDC`, `..._SLACK`)
  falling back to the instance-wide value, or a map key on
  `OAuthUsersConfig` (`per_provider.oidc.auto_create_users`). The env-variable
  form is more consistent with the rest of `internal/config/config.go`, but the
  `envTransform` prefix rules do not currently produce map keys — check before
  committing to it.
- Whatever the shape, the per-provider default role must go through the same
  `KnownRoles` validation `OAuthUsersConfig.Validate()` applies, and the legacy
  alias resolution in `applyAuthProvisioningAliases` must keep working for the
  instance-wide pair.
- Tests: extend `internal/config/auth_provisioning_test.go` (provider override
  wins over instance-wide, instance-wide wins over default) and
  `internal/api/oauth_test.go` (a provider whose auto-creation is off is refused
  while another provider's login still provisions).
- Docs: the "User auto-provisioning" section of
  `website/docs/configuration/index.md`, plus the env tables in `CLAUDE.md` and
  `README.md`.
