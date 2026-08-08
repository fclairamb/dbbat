---
model: sonnet
effort: medium
---

# 429 is declared on 33 arbitrary operations; it should be declared on 6 deliberate ones

No GitHub issue filed yet — file one before implementing.

## Goal

Stop documenting `429` as a per-operation response. It is a cross-cutting
middleware outcome that can happen on **any** endpoint, so enumerating it per
operation is noise that is guaranteed to be incomplete and misleading.

Declare it only where a client must handle it *specially* — the
session-validation path, where mistaking a 429 for something else logs a user
out or tells them their password is wrong.

## Why

This replaces an earlier draft of this spec, which proposed the opposite: add
`429` to the 37 authenticated operations that lack it, so all 77 declare it
uniformly. That is the wrong direction.

`internal/api/server.go` (~line 292) installs `PostAuthMiddleware()` on the
whole `authenticated` group, and `PreAuthMiddleware()`/`Middleware()` cover the
rest, so *every* operation in the API can answer 429. A response that is
universally possible carries no information when written next to one specific
operation — it only invites the reader to infer that the operations **without**
it cannot rate-limit, which is false.

The current state is not even consistently wrong: **33 of 77 operations declare
it**, and the 33 are arbitrary — `createDatabase`, `listUsers`, `getQuery` and
`deleteConnectionDump` do, while `listGrantDefinitions`, `approveQuery` and
`POST /auth/logout` do not. Nobody chose that split; it accreted.

The one place the declaration earns its keep is the auth bootstrap. That is the
whole point of
[specs/done/2026/08/2026-08-07-01-429-must-not-log-out.md](../done/2026/08/2026-08-07-01-429-must-not-log-out.md):
a 429 on `GET /auth/me` was read as "this token is invalid" and destroyed a
valid session, and a 429 on `POST /auth/login` rendered as a generic "Login
failed". On those endpoints the 429 is not ambient noise — it is a distinct
outcome the client must branch on, and it belongs in the contract.

## Decision

**Keep `429` on exactly these six operations:**

| operationId | path | why it must be declared |
|---|---|---|
| `getCurrentUser` | `GET /auth/me` | a 429 misread as invalid-token destroys a live session |
| `login` | `POST /auth/login` | a 429 misread as bad credentials shows the wrong error |
| `logout` | `POST /auth/logout` | a 429 leaves the user believing they disconnected when they did not |
| `changePasswordPreLogin` | `PUT /auth/password/pre-login` | same bootstrap path, same failure mode |
| `oauthExchange` | OAuth code exchange | a 429 mid-exchange must not present as a failed login |
| `deviceAuthorization` | device-code authorization | same |

**Strip `429` from the other 27 operations that currently declare it**, and do
**not** add it to the 37 that don't.

## Implementation

1. In `internal/api/openapi.yml`, remove the `'429':` response entry from every
   operation except the six above. That is 27 deletions of a two-line block.
2. State the convention once, where a reader will find it — the spec's
   top-level `description`, next to the existing auth notes. Something like:
   *"Every endpoint is rate limited and may answer `429` with the standard
   `Error` body (`code: RATE_LIMITED`) and a `Retry-After` header. It is
   documented per-operation only where the client must branch on it rather than
   simply retry — the session-validation endpoints under `/auth`."*
   Keep the `RateLimited` response component: the six keep referencing it.
3. Regenerate the client: `cd front && bun run generate-client`. Confirm
   `front/src/contexts/AuthContext.tsx` still compiles — it gates on the raw
   `response.response.status`, not the typed error, so it must be unaffected.
   That independence is deliberate; do not "improve" it into using the
   generated 429 type.
4. Guard it in `internal/api/openapi_parity_test.go`, which already walks the
   gin route table against the spec. Add a third assertion next to
   `TestOpenAPIRouteParity` / `TestOpenAPIOperationIDs`: the set of operations
   declaring `429` equals the six-operation allowlist, named literally in the
   test. A hardcoded list is the right shape here — the point is that adding a
   seventh is a deliberate act someone has to justify in review, not something
   that drifts back in.

## Out of scope

The `Error` schema's `code` enum is missing values the API actually emits —
`CONFLICT`, `OAUTH_FAILED`, `OAUTH_STATE_MISMATCH`, `OAUTH_PROVIDER_ERROR`,
`OAUTH_USER_NOT_LINKED`, `OAUTH_EXCHANGE_INVALID`, `OAUTH_WRONG_WORKSPACE` (see
the `ErrCode*` constants in `internal/api/errors.go`). That is a real gap and
worth a test asserting the enum matches the constant set, but it is unrelated
to the 429 question — split it out rather than bundling it here.
