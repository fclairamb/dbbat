# 37 authenticated operations can answer 429 but don't declare it in OpenAPI

No GitHub issue filed yet — file one before implementing.

## Goal

Every operation registered under the `authenticated` route group declares
`429` in `internal/api/openapi.yml`, so the generated
`front/src/api/schema.ts` types rate limiting uniformly instead of on 40
of 77 operations.

## Why

Found while implementing
[2026-08-08-auth-me-429-openapi-drift.md](2026-08-08-auth-me-429-openapi-drift.md),
which fixed exactly one operation (`GET /auth/me`) and then audited the
rest.

`internal/api/server.go` (~line 292) installs
`s.rateLimiter.PostAuthMiddleware()` on the whole `authenticated` group,
so **every** authenticated operation can answer 429. The spec declares it
on 40 of 77 operations; 37 authenticated ones are silent about it:

```
DELETE /grant-definitions/{uid}          GET  /queries/pending
DELETE /parameters/{group}/{key}         GET  /servers/{uid}/connection
DELETE /user-groups/{uid}                GET  /ssh-servers
DELETE /user-groups/{uid}/members/{user_uid}  GET  /stream
GET    /auth/device/consent              GET  /user-groups
GET    /grant-definitions                GET  /user-groups/{uid}
GET    /grant-definitions/{uid}          GET  /user-groups/{uid}/members
GET    /grant-requests                   PATCH /grant-definitions/{uid}
GET    /grant-requests/{uid}             PATCH /user-groups/{uid}
GET    /instance                         POST /auth/device/consent
GET    /parameters                       POST /auth/logout
GET    /parameters/{group}/{key}         POST /grant-definitions
POST   /grant-definitions/validate-patterns   POST /queries/{uid}/approve
POST   /grant-requests                   POST /queries/{uid}/deny
POST   /grant-requests/{uid}/approve     POST /servers/{uid}/test
POST   /grant-requests/{uid}/cancel      POST /user-groups
POST   /grant-requests/{uid}/deny        PUT  /instance/public
POST   /queries/pending/deny-all         PUT  /parameters/{group}/{key}
                                         PUT  /user-groups/{uid}/members/{user_uid}
```

It was kept out of the `/auth/me` change deliberately: 37 operations plus
the regenerated `schema.ts` is a large diff that deserves its own review,
and it is pure mechanics with no behaviour change to reason about.

`GET /stream` is worth a thought rather than a blind `$ref`: it is a
WebSocket upgrade, so a 429 there is a rejected *handshake*, not a
message on the socket. Document it, but check the description says that.

## Implementation

1. Add `'429': { $ref: '#/components/responses/RateLimited' }` to each
   operation above in `internal/api/openapi.yml`. Use `RateLimited`, not
   `AuthRateLimited` — the latter describes the pre-auth login limiter
   ("Too many failed authentication attempts") and omits the
   `X-RateLimit-*` headers that `PostAuthMiddleware` sets. Both resolve to
   the same `Error` schema, so the generated types are identical either
   way; the difference is documentation accuracy.
2. Regenerate the client: `cd front && bun run generate-client`.
3. Consider guarding this with a test in
   `internal/api/openapi_parity_test.go` — it already walks the gin route
   table and the spec side by side, so "every route under the
   `authenticated` group declares 429" is a natural third assertion next
   to `TestOpenAPIRouteParity` and `TestOpenAPIOperationIDs`. That is what
   stops the drift coming back. It needs a way to tell which registered
   routes carry `PostAuthMiddleware`; gin's `RouteInfo` exposes only the
   final handler name, so this probably means asserting against the same
   path list the router builds rather than sniffing middleware.
4. While in the file: the `Error` schema's `code` enum is missing values
   the API actually emits — `CONFLICT`, `OAUTH_FAILED`,
   `OAUTH_STATE_MISMATCH`, `OAUTH_PROVIDER_ERROR`, `OAUTH_USER_NOT_LINKED`,
   `OAUTH_EXCHANGE_INVALID`, `OAUTH_WRONG_WORKSPACE` (see the `ErrCode*`
   constants in `internal/api/errors.go`). Add them, and ideally assert the
   enum matches the constant set in a test so it cannot drift again.
