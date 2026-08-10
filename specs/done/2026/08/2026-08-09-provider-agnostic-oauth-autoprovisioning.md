# Provider-agnostic OAuth auto-provisioning settings

## Goal

Move the two auto-provisioning knobs off `SlackAuthConfig` so they read as what
they are — settings for *every* OAuth/OIDC provider — instead of Slack ones the
generic OIDC provider happens to borrow.

## Why

`findOrCreateOAuthUser` (`internal/api/oauth.go`) gates user creation on
`s.config.SlackAuth.AutoCreateUsers` and takes the default role from
`s.config.SlackAuth.DefaultRole`, whatever provider vouched for the user. Since
the generic OIDC provider landed (`internal/auth/oidc`), an operator who has
never touched Slack has to set `DBB_SLACK_AUTH_AUTO_CREATE_USERS=false` to stop
their OIDC issuer from auto-creating accounts. That is a documentation trap and
a support ticket waiting to happen — `website/docs/configuration/sso.md` has to
explain it in prose today.

No GitHub issue yet — file one when picking this up.

## Implementation

- Add `DBB_AUTH_AUTO_CREATE_USERS` and `DBB_AUTH_DEFAULT_ROLE` (new
  `OAuthUsersConfig`, or fields directly on `Config`) in
  `internal/config/config.go`, defaulting to today's values (`true`,
  `connector`).
- Keep `DBB_SLACK_AUTH_AUTO_CREATE_USERS` / `DBB_SLACK_AUTH_DEFAULT_ROLE`
  working as aliases — deployments in the wild set them. Resolution order:
  explicit canonical wins, then the legacy alias, then the default. Mirror the
  `DBB_SLACK_SIGNING_SECRET` alias handling already in `Load()` (including its
  explicit `k.Set` re-apply, since koanf gives no ordering guarantee when both
  are present).
- Read the resolved values in `findOrCreateOAuthUser`
  (`internal/api/oauth.go`, the `s.config.SlackAuth.AutoCreateUsers` branch).
- Consider a per-provider override later; not needed for this change.
- Tests: extend `internal/config/oidc_test.go` (or a new
  `auth_provisioning_test.go`) with canonical-wins, alias-only, and
  neither-set cases. `internal/api/oauth_test.go` already builds a
  `config.Config` per test — switch those to the new field.
- Docs: `CLAUDE.md` + `README.md` env tables,
  `website/docs/configuration/index.md`, and drop the borrowed-knob paragraph
  from `website/docs/configuration/sso.md`.
