---
model: sonnet
effort: medium
---

# Admin API-key list should show the owning user's name (and all users' keys)

Originating issue: [#248](https://github.com/fclairamb/dbbat/issues/248)

## Problem
All API keys are visible to the admin user (expected), but the list does not
show which user owns each key. There is no way for an admin to see, per key,
whose key it is.

## Proposal

### Frontend
- Add an admin-only toggle "See other users' keys" on the API keys page
  (`front/src/routes/_authenticated/keys*` — mirror the existing admin toggles
  used elsewhere, e.g. the grants/connections `getUserName(uid)` helper at
  `front/src/routes/_authenticated/grants/index.tsx:135`).
- When enabled, list every user's keys, each row showing the owning user's
  name/username (resolve via the users list, same pattern as grants and
  connections pages).
- Optionally, add a per-user filter (dropdown of users) so an admin can narrow
  to a single user instead of the full list.
- Non-admins keep seeing only their own keys; the toggle is hidden for them.

### API / backend
Current behaviour (`internal/api/keys.go` `handleListAPIKeys`): for admins, when
no `user_id` query param is given, `filter.UserID` is left nil and
`store.ListAPIKeys` (`internal/store/api_keys.go:366`) returns **every user's
keys**. So the endpoint always dumps all keys for an admin by default — that's
what we want to stop doing.

Change so the endpoint doesn't always return all keys:
- **Default for every caller (no filter params): only the caller's own keys.**
  Pin `filter.UserID` to the current user by default, including for admins.
- Admin-only filters (ignored for non-admins, who stay pinned to their own UID —
  no cross-user access):
  - `user_id=<uuid>` — return one specific user's keys. Already implemented for
    admins; keep it, but it now overrides the own-keys default rather than being
    the only way to scope.
  - `all_users=true` (new) — return every user's keys, to back the "See other
    users' keys" toggle. Honoured only for admins.
- **Naming:** do **not** reuse the existing `include_all` query param for this —
  it already maps to `APIKeyFilter.IncludeAll` (`internal/store/models.go:545`),
  which means "include revoked/expired keys" and is orthogonal to user scoping.
  Use a distinct name such as `all_users`.
- Keep returning each key's `user_id` so the frontend can join to user names.
- Update the OpenAPI spec (`internal/api/openapi.yml`) to document `user_id`, the
  new `all_users` param, and the own-keys-by-default behaviour.

### Tests
- Extend `internal/store/api_keys_test.go` and the API handler tests: admin with
  no params sees only own keys; `all_users=true` returns all users' keys;
  `user_id=X` returns that user's keys; a non-admin passing `all_users=true` or
  `user_id=X` still only ever sees their own keys.

## Implementation Plan

### Backend

1. **`internal/api/keys.go` — `handleListAPIKeys`**
   - Rewrite the scoping block: default `filter.UserID = &currentUser.UID` for
     *every* caller (admin or not).
   - For admins only, override the default:
     - `all_users=true` → leave `filter.UserID` nil (all users' keys).
     - `user_id=<uuid>` → parse and set `filter.UserID` to that value; takes
       precedence over `all_users` if both are present (most-specific wins).
   - Non-admins: both params are ignored outright (never read `c.Query` for
     them), so a non-admin can never see another user's keys.
   - `store.ListAPIKeys` / `APIKeyFilter` need no changes — `UserID: nil` already
     means "all users" and `UserID: &x` already scopes to one user; the schema
     change is entirely which UserID the handler decides to pass in.

2. **`internal/api/openapi.yml`** — `/keys` GET:
   - Update the `description` to state the new default (own keys only) and
     document precedence between `user_id` and `all_users`.
   - Add a new `all_users` boolean query parameter (admin only).
   - Update `user_id`'s description to reflect that it now overrides the
     own-keys default rather than being the only way to scope (still admin-only).

### Frontend

Resolve owner names client-side via the existing `useUsers()` list — mirrors
the `getUserName(uid)` pattern already used on `grants/index.tsx` and
`connections/index.tsx`. No backend enrichment of `APIKey` is needed since the
response already includes `user_id`.

3. **`front/src/api/queries.ts` — `useAPIKeys`**: add `all_users?: boolean` to
   the filters type so it's passed through to `GET /keys` as a query param
   (mirrors the existing `user_id` / `include_all` params already wired).

4. **`front/src/routes/_authenticated/api-keys/index.tsx`**:
   - Admin-only controls (gated on `hasRole(user?.roles, 'admin')`, hidden for
     non-admins): a "See other users' keys" `Switch` toggle, and — only when
     the toggle is on — a `Select` dropdown of users (from `useUsers()`) to
     narrow to one user; "All users" is the default option when the toggle is
     on with no user picked.
   - State: `showAllUsers` (bool) and `filterUserId` (string | undefined).
     Pass `{ all_users: showAllUsers && !filterUserId, user_id: filterUserId }`
     into `useAPIKeys(...)`.
   - Add an "Owner" column (via `getUserName(k.user_id)`, same helper pattern
     as grants/connections) that only renders when `showAllUsers` is on (own
     keys view doesn't need it — it's always the current user).
   - Non-admins: toggle and column stay hidden entirely; `useAPIKeys()` called
     with no filters (defaults to own keys server-side anyway).

5. Regenerate `front/src/api/schema.ts` via `bun run generate-client` (or hand
   -sync the `all_users` param) after the openapi.yml change, so the generated
   client types include the new query parameter.

### Tests

6. **`internal/api/keys_test.go`** (new handler-level tests, following the
   `setupTestServer` / `createTestUser` / `loginUser` pattern from
   `users_test.go` / `password_reset_test.go`; mount
   `router.Use(server.authMiddleware()); router.GET("/api/v1/keys",
   server.handleListAPIKeys)`):
   - Admin, no query params → sees only own keys.
   - Admin, `all_users=true` → sees all users' keys.
   - Admin, `user_id=<other>` → sees only that user's keys.
   - Admin, both `user_id` and `all_users=true` → `user_id` wins.
   - Non-admin, `all_users=true` → still only own keys.
   - Non-admin, `user_id=<other>` → still only own keys (not forbidden, just
     ignored/pinned to self).

7. Re-verify `internal/store/api_keys_test.go`'s existing `TestListAPIKeys`
   already covers `UserID: nil` → all keys and `UserID: &x` → scoped, which is
   sufficient store-level coverage since no store code changes; no edits
   needed there unless the review step finds a gap.

### QA
8. `make build-backend lint-back test`.
9. `make build-front` (or `cd front && bun run lint`) — confirm the frontend
   compiles/typechecks and lints clean on the touched file.
10. Manually sanity-check in the running dev instance (admin toggle appears,
    non-admin doesn't see it, filtering works) since Playwright E2E for this
    page isn't already scaffolded — author a small E2E case only if an
    existing `api-keys` or `keys` spec file is found under `front/e2e/`.
