# OAuth error path still loses the post-login redirect target

## Goal
When a Slack sign-in attempt fails (state mismatch, provider error, code
exchange failure, unlinked user), keep the `redirect` target alive so that a
retry from the login page still lands the user where they started (e.g. the
device consent page).

## Why
The happy path was fixed (the redirect target now rides the `oauth_states`
row from `/auth/slack` to the callback's `/login?token=...&redirect=...`
redirect — see `internal/api/oauth.go`). But every error branch goes through
`redirectWithError`, which emits `/login?error=<code>` with no `redirect`
param. On top of that, the login page strips the `error` param with
`history.replaceState`, so by the time the user clicks "Sign in with Slack"
again, `redirectTarget` has fallen back to `/` and the original destination
is gone.

## Implementation
- `internal/api/oauth.go`: thread the sanitized redirect target into
  `redirectWithError` (append `&redirect=...`). For branches that run after
  `ConsumeOAuthState`, use `oauthState.RedirectURL`; the earlier branches
  (missing/unknown state) have nothing to forward and can stay as-is.
- `front/src/routes/login.tsx`: `getSafeRedirectTarget` already picks the
  param up at mount; no change needed there since the value is captured
  before `replaceState` strips it.
- Tests: extend `TestOAuthLoginRedirectRoundTrip` in
  `internal/api/oauth_test.go` with a failing `ExchangeCode` case asserting
  the error redirect keeps the param.

No GitHub issue filed yet — one should be created.
