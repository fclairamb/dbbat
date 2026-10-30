---
model: opus
effort: low
---

# Expired oauth_states rows are never deleted, and unauthenticated requests create them

## Problem

Found by the security scan of 2026-10-30 (finding M9).

Severity: Medium. Triggered by an unauthenticated network client.

`Store.CleanupExpiredOAuthStates` exists (`internal/store/oauth_states.go:65`) but nothing calls it. Every row stays forever: OAuth authorize state, device-authorization requests, login-exchange codes. Unauthenticated endpoints create them:

- `POST /api/v1/auth/device` creates a device-authorization row (`internal/store/device_auth.go:91`) and appends a chained `device_auth.requested` audit entry. It is behind `PreAuthMiddleware`, which is keyed on the client IP, and that IP can be spoofed (finding L1).
- `GET /api/v1/auth/<provider>` creates a state row on every request (`internal/api/oauth.go:201`) and has no rate limiter at all (`internal/api/server.go:273-276`).

A client looping on these grows `oauth_states` and `audit_log` without bound: disk growth on the store, a longer audit chain to verify, and a slower device flow. Each new device request runs `deviceAuthUserCodeExists`, which filters on `metadata->>'user_code'` (`device_auth.go:106-117`) over every row.

## Proposal

1. Call `CleanupExpiredOAuthStates` periodically from the existing maintenance loop (`retention.go` / `heartbeat.go`, under `safe.RunMaintenance`), for example every 10 minutes, whatever the retention settings.
2. Put `PreAuthMiddleware` in front of the OAuth authorize routes too.
3. Optional: an expression index on `(provider, (metadata->>'user_code'))` for the device lookup.

## Tests

- Store test: expired rows of each provider namespace are deleted by `CleanupExpiredOAuthStates` and live ones are kept (likely exists; extend if not).
- Test that the maintenance job is registered and calls the cleanup. This fails before the fix.
- API test: the OAuth authorize route answers 429 once the anonymous per-IP limit is hit.
