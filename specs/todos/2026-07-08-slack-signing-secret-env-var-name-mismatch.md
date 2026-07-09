# Slack signing-secret env var name mismatch (docs vs. code)

## Goal

Make the Slack signing-secret environment variable name consistent between the
documentation and the code. Today they disagree, so following the docs produces a
silently non-working deployment.

## Why

The spec, `CLAUDE.md` env table, and `website/docs/configuration/index.md` all
document the variable as **`DBB_SLACK_SIGNING_SECRET`**. But the code puts
`SigningSecret` inside `SlackNotifyConfig`:

- `internal/config/config.go` — `SigningSecret string ` + `SlackNotify SlackNotifyConfig `
- the env transform maps `slack_notify_*` → `slack_notify.*` (config.go ~L417)

so the value is only read from **`DBB_SLACK_NOTIFY_SIGNING_SECRET`**
(`DBB_` + `slack_notify_signing_secret`). Setting the documented
`DBB_SLACK_SIGNING_SECRET` has no effect → `Interactive()` stays false → the
`POST /api/v1/slack/interactions` route is never registered and messages carry no
buttons. The endpoint returns `404` instead of `401`.

Discovered live: on the Stonal deployment, setting `DBB_SLACK_SIGNING_SECRET` left
the endpoint at `404`; renaming the env var to `DBB_SLACK_NOTIFY_SIGNING_SECRET`
flipped it to `401` and enabled interactivity. The completeness audit missed this
because it verified the field existed and was koanf-mapped, but didn't cross-check
the resulting env var name against the documented one.

## Implementation

Pick ONE direction and make everything agree:

**Option A (recommended) — align the code to the documented name `DBB_SLACK_SIGNING_SECRET`.**
The signing secret is conceptually about interactivity, not just notifications, so
the shorter documented name is the better public contract.
- Either move `SigningSecret` out of `SlackNotifyConfig` to a top-level
  `Slack`/root field mapped to `signing_secret` with an env special-case, or add an
  explicit alias in the `envTransform` in `internal/config/config.go` so
  `slack_signing_secret` → `slack_notify.signing_secret`.
- Keep `Interactive()` semantics (signing secret + bot token both set).
- Add a config unit test asserting `DBB_SLACK_SIGNING_SECRET` populates
  `SlackNotify.SigningSecret` (this is the assertion the audit lacked).

**Option B — change the docs to `DBB_SLACK_NOTIFY_SIGNING_SECRET`.**
Update `CLAUDE.md` env table, `website/docs/configuration/index.md`, the
`slack_app_manifest.json` setup notes, and the archived spec
`specs/done/2026/07/2026-07-07-slack-interactive-grant-approval.md`. Lower effort
but leaves a slightly inconsistent naming family (`DBB_SLACK_SIGNING_SECRET` would
have read more naturally alongside `DBB_SLACK_AUTH_*`).

Either way: grep the tree for `DBB_SLACK_SIGNING_SECRET` and
`DBB_SLACK_NOTIFY_SIGNING_SECRET` and make all references (code, tests, docs,
manifest, openapi) consistent.

No GitHub issue filed yet — one should be opened and linked here.
