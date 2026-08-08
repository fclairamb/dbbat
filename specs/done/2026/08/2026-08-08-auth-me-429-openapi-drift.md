# GET /auth/me can answer 429 at runtime but the OpenAPI spec doesn't say so, and its body shape doesn't match the Error schema

No GitHub issue filed yet — file one before implementing.

## Goal

Bring `GET /auth/me`'s documented contract in line with what it actually
does under load, and make its 429 body consistent with every other
rate-limited endpoint.

## Why

Found while implementing
[specs/todos/2026-08-07-01-429-must-not-log-out.md](2026-08-07-01-429-must-not-log-out.md).

`GET /auth/me` is registered under the `authenticated` route group in
[internal/api/server.go](../../internal/api/server.go) (`authenticatedAuth.GET("/me", s.handleMe)`,
around line 300), which runs `RateLimiter.PostAuthMiddleware()` — so it
genuinely can return 429. But:

- [internal/api/openapi.yml](../../internal/api/openapi.yml)'s `/auth/me`
  operation (`getCurrentUser`, around line 191) only declares `200` and
  `401`. No `429`. That means the generated `front/src/api/schema.ts`
  types `response.error` for this call without the 429/`AuthRateLimited`
  shape at all — the frontend fix for the sibling todo above had to rely
  on the raw HTTP status code rather than the typed error, which happens
  to be the more robust choice anyway, but the type gap is still real and
  will silently mislead the next person who reads this endpoint's types.
- The body `PostAuthMiddleware` actually writes
  ([internal/api/ratelimit.go](../../internal/api/ratelimit.go) lines
  235-249) is an ad-hoc `gin.H{"error": "rate_limit_exceeded", "message":
  ..., "retry_after": ...}` — a different shape from the `Error` schema
  (`code`/`message`/`detail`/`retry_after`) that `writeRateLimited()` in
  [internal/api/errors.go](../../internal/api/errors.go) uses for the
  pre-auth rate limiter (login, password change, etc.). Same is true of
  the plain `RateLimiter.Middleware()` (lines 148-200) used elsewhere.
  Anything parsing `error.code === "RATE_LIMITED"` against a
  `PostAuthMiddleware`-guarded endpoint will silently never match.

## Implementation

1. Add `'429': { $ref: '#/components/responses/AuthRateLimited' }` to the
   `/auth/me` `get` operation in `internal/api/openapi.yml`, then
   `bun run generate-client` (or `make build-front`) to regenerate
   `front/src/api/schema.ts`.
2. Make `PostAuthMiddleware` (and the plain `Middleware()`) emit the same
   `ErrorBody{Code: ErrCodeRateLimited, Message, RetryAfter}` shape as
   `writeRateLimited()` — ideally by calling `writeRateLimited()` itself
   instead of hand-rolling `gin.H{...}` — so every 429 in the API has one
   consistent JSON shape.
3. Check `internal/api/ratelimit_test.go` / `middleware_test.go` for
   assertions on the old `{"error": "rate_limit_exceeded", ...}` shape
   and update them.
4. Re-run `make test` and skim other frontend call sites that already
   branch on `error.code` for a 429 they get from an authenticated
   endpoint (should be none today, but worth grepping) to make sure
   nothing was quietly relying on the old shape.
