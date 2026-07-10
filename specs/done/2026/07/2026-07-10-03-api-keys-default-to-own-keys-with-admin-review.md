# API keys page mixes everyone's keys together; users should see their own by default, admins should get a review view

## Problem

`/app/api-keys` is clunky. The backend list endpoint
([`internal/api/keys.go:86`](internal/api/keys.go:86)) already scopes
non-admins to their own keys, but for admins it returns **every user's keys**
in one flat list. The frontend page
([`front/src/routes/_authenticated/api-keys/index.tsx:55`](front/src/routes/_authenticated/api-keys/index.tsx:55))
calls `useAPIKeys()` with no filter, so an admin lands on a single table
mixing their own keys with everyone else's — and since the table has no
owner column (columns are name, prefix, status, expires, last used, requests
— [lines 70–146](front/src/routes/_authenticated/api-keys/index.tsx:70)),
there is no way to tell whose key is whose.

Two distinct needs are conflated in one view:

1. **Self-service**: "manage *my* keys" — what every user (admin included)
   wants by default.
2. **Fleet review**: admins need to review all provisioned keys across users,
   specifically to spot **long-term keys** — keys with no expiry
   (`expires_at = null`, rendered as "Never") or a far-future expiry — which
   are a standing security risk.

The API already supports the split: `GET /keys` takes `user_id` and
`include_all` query params ([`internal/api/keys.go:91-109`](internal/api/keys.go:91)),
and the `useAPIKeys` hook already accepts both filters
([`front/src/api/queries.ts:749`](front/src/api/queries.ts:749)). The page
just never uses them.

## Proposal

### Default view: own keys only (everyone)

- The page loads with the list scoped to the current user. For non-admins
  nothing changes (backend already enforces this); for admins, pass
  `user_id=<current user>` so their default view matches everyone else's.

### Admin-only "All keys" review view

- For admins, add a scope switcher (e.g. tabs or a toggle: **My keys** /
  **All keys**) — hidden entirely for non-admins.
- In the "All keys" view:
  - Add an **Owner** column. The `APIKey` model only carries `user_id`
    ([`internal/store/models.go:399`](internal/store/models.go:399)), so
    either enrich the list response with the owner's login/name (preferred —
    avoids an N+1 and works even if the admin users list ever gets paginated),
    or map `user_id` → login client-side via the existing users query.
  - Make long-term keys easy to spot: visually flag keys whose `expires_at`
    is null ("Never") or beyond some threshold — e.g. a warning-styled badge
    on the Expires cell, and/or sort them first. This is the review use case:
    "is there any long-lived key that shouldn't exist?"
- Admins can already revoke any key
  ([`internal/api/keys.go:163`](internal/api/keys.go:163)), so the existing
  revoke action in the row works unchanged in the review view.

### Open questions

- Threshold for "long term": is "never expires" the only flag, or also
  "expires more than N days out" (e.g. > 90 days)? Start with flagging
  `expires_at = null` and keep the threshold easy to adjust.
- Should the "All keys" view default `include_all=true` (show revoked/expired
  for audit) or stay consistent with the current default? Suggest keeping the
  current default and leaving `include_all` out of scope.

### Acceptance / verification

- Non-admin (`viewer`/`connector` test users): sees only their own keys; no
  scope switcher visible.
- Admin: page opens on "My keys" showing only the admin's own keys.
- Admin switches to "All keys": every user's non-session key is listed with
  an Owner column; keys with no expiry are visually flagged.
- Revoke still works from both views, with the existing confirmation dialog.
- E2E: extend/add a Playwright spec under `front/e2e/` covering the default
  self-scope for admin, the scope switch, the owner column, and the
  long-term-key flag (test mode provisions stable keys —
  [`internal/store/api_keys.go:116`](internal/store/api_keys.go:116)).

## Implementation Plan

### Backend (owner enrichment — the "preferred" option)

1. **`internal/store/models.go`** — add a non-persisted display field to the
   `APIKey` model: `UserLogin string \`bun:"-" json:"user_login,omitempty"\``.
   It is not a DB column; it is populated only by the list handler for admins.
2. **`internal/store/users.go`** — add
   `GetUsernamesByIDs(ctx, ids []uuid.UUID) (map[uuid.UUID]string, error)`:
   a single `WHERE uid IN (...)` batch lookup (no N+1, unaffected by any future
   `/users` pagination). Soft-deleted owners are simply absent from the map.
3. **`internal/api/keys.go`** — in `handleListAPIKeys`, after listing keys and
   only when the caller is an admin, collect the distinct `user_id`s, call
   `GetUsernamesByIDs`, and set `keys[i].UserLogin`. Best-effort: a lookup error
   does not fail the request (owner just renders as a fallback).
4. **`internal/api/openapi.yml`** — add the optional `user_login` property to the
   `APIKey` schema, then regenerate the frontend types
   (`cd front && bun run generate-client`).

### Frontend (`front/src/routes/_authenticated/api-keys/index.tsx`)

5. **Default to own keys.** Read `{ user, isAdmin }` from `useAuth()`. Hold a
   `scope: "mine" | "all"` state (default `"mine"`). Non-admins are always
   `"mine"`. Call `useAPIKeys(scope === "mine" ? { user_id: user?.uid } : undefined)`
   so the page opens on the current user's keys for everyone.
6. **Admin scope switcher.** Render a small segmented **My keys / All keys**
   toggle (two `Button`s) only when `isAdmin`; hidden for non-admins.
7. **Owner column.** When `isAdmin && scope === "all"`, insert an **Owner**
   column after Name that renders `k.user_login` (fallback to `k.user_id`).
8. **Long-term-key flag.** Add an `isLongTermKey(k)` helper (currently
   `!k.expires_at` — "Never"; structured so a `> N days` threshold is a
   one-line change). In the Expires cell, render a warning-styled amber badge
   with an `AlertTriangle` icon for flagged keys instead of the muted "Never".
   In the "All keys" view, stable-sort flagged keys first.

### Tests

9. **`internal/store/users_test.go`** — unit test for `GetUsernamesByIDs`
   (multiple ids, missing id, empty input).
10. **`front/e2e/api-keys.spec.ts`** — new Playwright spec: admin lands on
    "My keys" (only `admin-test-key`), no scope switcher for non-admins is
    implicitly covered by admin-only rendering; switch to "All keys" shows all
    three seeded keys with an Owner column (admin/connector/viewer) and the
    long-term "Never" flag; revoke dialog still opens. Authored to run under
    `DBB_RUN_MODE=test`.
