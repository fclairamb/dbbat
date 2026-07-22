# CLI authorization flow (device-flow style API key provisioning)

## Goal

Let any command-line tool or script obtain a `dbb_` API key for a user without manual
copy/paste, through a browser-approved, device-authorization-style flow (in the spirit of
RFC 8628, as popularized by `gh auth login`):

1. The CLI calls an unauthenticated endpoint to open an authorization request and gets back
   an `authorize_url`, a secret `poll_token`, and a short human-checkable `user_code`.
2. The CLI opens the browser at `authorize_url` (or prints it when headless). The page sits
   behind the normal login (SSO or password) and shows the request name + `user_code` with
   Approve / Deny buttons.
3. On approval, the server mints a real `dbb_` key for the approving user.
4. The CLI polls with its `poll_token` and receives the key exactly once.

## Why

Today an API key can only be created from the web UI or with Basic Auth. SSO-provisioned
users have no password, so tooling cannot self-provision keys: every CLI/script user must
log into the UI, create a key, and paste it by hand. A generic browser-approval flow makes
key provisioning scriptable while keeping a human in the loop, works over SSH (no localhost
callback needed), and never exposes the key in a browser URL.

No GitHub issue filed yet — one should be created when picking this up.

## Implementation

### Storage — reuse `oauth_states`, no migration

Pending requests are `oauth_states` rows with `provider = 'cli'`
([models.go:734](../../internal/store/models.go), [oauth_states.go](../../internal/store/oauth_states.go)):

- `uid` → public request id, used in `authorize_url` (knowing it does not yield the key)
- `state` (unique, indexed) → the `poll_token` secret (high-entropy, CLI-only)
- `expires_at` → 10-minute TTL; `CleanupExpiredOAuthStates` already garbage-collects
- `metadata` jsonb → `{name, user_code, status: pending|approved|denied, encrypted_key}`

`provider='cli'` cannot collide with real OAuth providers: `/auth/:provider` routes are only
registered for configured providers. Add a comment on the model noting the table now holds
generic short-lived auth-flow state, not only OAuth CSRF states.

The store needs one new capability next to `ConsumeOAuthState`: a conditional
metadata/status update (`UPDATE ... WHERE uid = ? AND metadata->>'status' = 'pending'`) so
approve/deny/poll transitions are race-free. Final key delivery reuses the consume-once
delete pattern.

### API — new `internal/api/cliauth.go`

- `POST /api/v1/auth/cli` (unauthenticated): body `{name}` (e.g. `"my-tool on host"`,
  length-capped). Creates the state row, returns
  `{authorize_url, poll_token, user_code, expires_at, interval}`.
- `POST /api/v1/auth/cli/:uid/respond` (gated by `requireWebSessionOrBasicAuth()`, same as
  key creation): body `{approve: bool}`. On approve: mint a key for the current user via
  `store.CreateAPIKey`, encrypt the plaintext with the master key (AES-256-GCM, AAD-bound
  to the state uid — same posture as Oracle verifier blobs), store it in `metadata`, set
  `status=approved`. On deny: `status=denied`.
- `POST /api/v1/auth/cli/poll` (unauthenticated): body `{poll_token}`. Returns
  `{status: pending|approved|denied|expired}`; when approved, also `{key}` and the row is
  deleted (delivered exactly once).

Update `internal/api/openapi.yml`, and emit audit events (`cli_auth.requested`,
`cli_auth.approved`, `cli_auth.denied`; `api_key.created` already fires from the store path).

The create endpoint uses the existing IP-based `RateLimiter.PreAuthMiddleware()`. The
poll endpoint deliberately skips it: a legitimate CLI polls every few seconds for up to
the request's TTL, which would blow through the anon per-minute budget; its abuse
resistance comes from the poll token's entropy instead (see handler comment).

### Frontend

New authenticated route `front/src/routes/_authenticated/cli-auth/$uid.tsx`: fetches the
request, displays the request `name` and `user_code` with "verify this code matches your
terminal", Approve / Deny buttons calling the respond endpoint. Unauthenticated visitors go
through the normal login redirect first.

### Security notes

- The key never transits a browser URL; only the opaque request uid does.
- `poll_token` never leaves the CLI; approval requires an authenticated web session.
- The `user_code` shown in both terminal and browser mitigates the classic device-flow
  phishing risk; keep the TTL short (10 min) and single-use.

### Testing

Playwright e2e (test mode, `admin`/`admintest`, password path — no SSO stubbing needed):
start a request via the API, log in, approve on the page, poll, assert the returned key
authenticates against `GET /api/v1/auth/me`. Plus Go unit tests for the state transitions
(expiry, deny, double-poll, poll-before-approve).

## Status

Implemented: `internal/store/cli_auth.go`, `internal/api/cliauth.go`, routes in
`internal/api/server.go`, `crypto.CLIAuthAAD`, OpenAPI schema/paths, and the frontend
approval page `front/src/routes/_authenticated/cli-auth/$uid.tsx`.

Tested: Go unit tests (`internal/store/cli_auth_test.go`,
`internal/api/cliauth_test.go`) cover create/get/respond/poll including expiry, deny,
double-respond, and delivered-exactly-once. Manually verified end-to-end in the browser
(`make dev`): create → approve → poll → key authenticates; create → deny → poll.

Deferred: the Playwright e2e spec described above was not written this round — the Go
API tests and manual browser verification covered the same paths for now. Add it as a
follow-up alongside the stn CLI's own testcontainers-based e2e test, tracked in
`specs/done/2026/07/2026-07-22-dbbat-login.md` (tool-stonal-cli repo) under "Still open".

Follow-up filed and since fixed:
`specs/done/2026/07/2026-07-22-auth-redirect-preserve-destination.md` — the
`_authenticated` login redirect didn't preserve the originally-requested URL, discovered
while testing this page (an unrelated, pre-existing limitation of the auth guard).
