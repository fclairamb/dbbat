# Standardize the CLI authorization flow as OAuth 2.0 Device Authorization Grant (RFC 8628)

## Goal

Replace the bespoke `/auth/cli` endpoints (shipped in
[cli-authorization-flow](../done/2026/07/2026-07-22-cli-authorization-flow.md))
with the standard **OAuth 2.0 Device Authorization Grant** (RFC 8628, the
"device flow" used by `gh auth login`, `aws sso login`, smart-TV sign-in).
Same mechanics, standard shape — so it's recognizable to any OAuth client
library and no longer names one particular client type.

## Why

Two problems with `/auth/cli`:

1. **The name over-commits to "CLI."** A desktop app (Electron, native,
   IDE plugin) is an equally valid initiator. Baking the client type into the
   URL is wrong — RFC 8628 is explicitly client-agnostic ("any client that
   is input-constrained or lacks a browser, *or otherwise*").
2. **The shape is bespoke.** `poll_token`, `authorize_url`, `request_id`,
   `{status: approved, key}` mean nothing to an OAuth client. The flow we
   built *is* structurally RFC 8628 — it just used invented names.

Doing it now is nearly free: the endpoints are merged on a branch but no real
external client consumes them yet (only the in-house `stn dbbat login`, also
unreleased). Renaming after a desktop app or third party integrates would be
a breaking change.

## Endpoint / field mapping

| bespoke (`/auth/cli`) | RFC 8628 (`/auth/device`) |
|---|---|
| `POST /auth/cli` `{name}` | `POST /auth/device` `{client_name}` |
| `authorize_url` | `verification_uri` + `verification_uri_complete` |
| `poll_token` | `device_code` |
| `user_code` (verify-only) | `user_code` (browser lookup key **and** verify) |
| `expires_at` / `interval_seconds` | `expires_in` / `interval` |
| `POST /auth/cli/poll` `{poll_token}` | `POST /auth/device/token` `{grant_type, device_code}` |
| `{status: pending}` | `400 {error: authorization_pending}` |
| `{status: denied}` | `400 {error: access_denied}` |
| `{status: expired}` | `400 {error: expired_token}` |
| `{status: approved, key}` | `200 {access_token, token_type: "Bearer"}` |
| `GET /auth/cli/:uid` | `GET /auth/device/consent?user_code=` |
| `POST /auth/cli/:uid/respond` `{approve}` | `POST /auth/device/consent` `{user_code, approve}` |

## Key model change: user_code is the browser identifier

RFC 8628 has no separate request UID. The browser side is keyed by
`user_code` (the human enters it at `verification_uri`, or opens
`verification_uri_complete` which pre-fills it); the client side is keyed by
the secret `device_code`. So:

- `oauth_states.state` (unique, indexed) = **device_code** (secret, client-only)
- `user_code` lives in `metadata` (canonical form, dashless uppercase, e.g.
  `WDJP4KXR`); the display form `WDJP-4KXR` is derived. Lookups normalize the
  input (uppercase, strip non-alphabet) before matching, so a user can type
  `wdjp-4kxr`, `WDJP4KXR`, etc.
- Internal `uid` stays as PK and is used for the race-safe conditional UPDATE
  on approve/deny (looked up via user_code, updated by uid).
- Create-time guard regenerates the `user_code` if a live row already holds it
  (belt-and-suspenders on top of ~40 bits of entropy; volume is tiny, 10-min TTL).

## Implementation

### dbbat

- `internal/crypto/encrypt.go`: `CLIAuthAAD` → `DeviceAuthAAD` (still binds the
  momentarily-held key to the request uid).
- `internal/store/cli_auth.go` → `device_auth.go`: provider `"cli"` → `"device"`,
  `Create/Get-by-user_code/Respond-by-user_code/Poll-by-device_code`, user_code
  normalize + format helpers, create-time uniqueness guard. Rename tests.
- `internal/api/cliauth.go` → `device.go`: the four handlers above. The token
  endpoint returns the OAuth error envelope `{error, error_description}` (a
  deliberate, documented deviation from dbbat's usual `{code, message}`), and
  on success `{access_token, token_type: "Bearer"}`. Body stays JSON (dbbat's
  API is JSON throughout) rather than RFC 8628's form-encoding — another
  documented, pragmatic deviation; the field names and error codes are what
  give the recognizability. Audit events `device_auth.requested|approved|denied`.
- `internal/api/server.go`: register `/auth/device`, `/auth/device/token`
  (both unauthenticated; the request endpoint keeps IP rate limiting, the token
  endpoint stays unlimited — the device_code is the capability), and the
  authenticated `/auth/device/consent` GET + POST (POST gated by
  `requireWebSessionOrBasicAuth`, same as key creation).
- OpenAPI spec + regenerate the TS client.
- Frontend: route `_authenticated/cli-auth/$uid.tsx` → `_authenticated/device.tsx`
  reading a `user_code` search param; when absent, show a code-entry field
  (the `verification_uri` path); when present, fetch consent details and show
  Approve/Deny. The existing login-redirect fix already carries the full
  `?user_code=…` deep link through login.

### tool-stonal-cli (`stn dbbat login`)

- `commands/dbbat/client.go` + `login.go`: call `POST /auth/device`
  (`{client_name}`), open `verification_uri_complete` (print `verification_uri`
  + `user_code` when headless), poll `POST /auth/device/token` with
  `{grant_type: "urn:ietf:params:oauth:grant-type:device_code", device_code}`,
  interpret the OAuth error codes (`authorization_pending`/`slow_down` → keep
  waiting, bumping the interval on `slow_down`; `access_denied` → denied;
  `expired_token` → expired), store the `access_token`. Keyring unchanged.

### Not doing (documented omissions)

- **Server-side `slow_down` enforcement** (tracking last-poll time to punish
  clients polling faster than `interval`). The spec makes it optional; our
  client respects `interval`. The server always answers `authorization_pending`
  while pending. Possible future enhancement.
- **Full OAuth AS surface** (`client_id` registration, form-encoded bodies, a
  general `/token` endpoint serving multiple grants). Out of scope — dbbat
  isn't a general authorization server. `client_id` is accepted-and-ignored if
  sent; `client_name` is the dbbat display extension.

## Testing

- Go unit tests (store + API): create → consent-by-user_code → approve/deny →
  token poll, covering `authorization_pending`, `access_denied`, `expired_token`,
  double-respond conflict, delivered-exactly-once, API-key-can't-approve, and
  user_code normalization.
- Frontend typecheck + lint.
- Manual browser e2e via `make dev`: open `verification_uri_complete`, log in,
  approve, confirm the token authenticates against `/auth/me`; plus the deny
  path and the manual code-entry page.
- `stn dbbat login` live e2e against the local dbbat (real HTTP approval),
  approve + deny paths.

## Status

Implemented and verified. dbbat: `internal/store/device_auth.go`,
`internal/api/device.go`, routes in `internal/api/server.go`,
`crypto.DeviceAuthAAD`, OpenAPI paths/schemas, and the frontend route
`front/src/routes/_authenticated/device.tsx` (consent + manual code-entry
page). tool-stonal-cli: `commands/dbbat/{client,login}.go` rewritten to the
device-flow endpoints. The old `/auth/cli*` endpoints, `cli_auth.go`,
`cliauth.go`, `CLIAuthAAD`, and the `cli-auth/$uid` route were removed —
supersedes [cli-authorization-flow](2026-07-22-cli-authorization-flow.md).

Tested: Go unit tests (store + API) all green under the CI-pinned
golangci-lint; frontend typecheck + lint clean; CLI unit tests green.
Manually verified end-to-end in the browser (`make dev`):
`verification_uri_complete` → login (deep link preserved) → consent →
approve → token poll returns Bearer token → authenticates as admin →
consumed-once; plus the manual code-entry page with lowercase/dashless
normalization, and the deny path. Live e2e with the compiled `stn` binary
against the running dbbat: `stn dbbat login --no-browser` → HTTP approval →
key stored → `status`/`key` confirm it authenticates.

Still deferred (unchanged from the original): a Playwright regression test,
and the CLI's testcontainers-based e2e test (blocked on a released dbbat
image).
