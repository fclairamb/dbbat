# Admin user-management UI: edit a user and promote/demote admin rights

## Goal

Give admins a user-management interface in the frontend to **update an existing
user** — most importantly to **promote a user to admin or demote them back**
(toggle the `admin` role). The page must be admin-only: non-admins keep the
current read-only view of themselves.

## Why

The backend already fully supports this, but the UI does not expose it:

- `PUT /api/v1/users/:uid` (`handleUpdateUser`, `internal/api/users.go:102`)
  accepts a `roles` array and enforces that only admins can change roles or
  touch other users (`internal/api/users.go:124-133`). Route registered in
  `internal/api/server.go:216`.
- Roles are `admin` / `viewer` / `connector` (`internal/store/models.go:14-16`).
- The frontend even has the plumbing: `useUpdateUser` mutation
  (`front/src/api/queries.ts:106`) and a `canUpdateUser` permission helper
  (`front/src/lib/permissions.ts:46`) — both currently unused by the users page.

Today the users page (`front/src/routes/_authenticated/users/index.tsx`) only
offers **create**, **delete**, and **reset password**. Admin rights can only be
chosen at creation time (checkbox mapping to `["admin"]` vs `["connector"]`);
there is no way to change a user's roles afterwards without hitting the API by
hand.

## Proposal

Frontend-only feature (plus small backend guardrails, see below), on the
existing users page:

1. **Edit action per row** — add a pencil/edit `PermissionButton` next to the
   existing reset-password and delete buttons, gated by
   `canUpdateUser(user?.roles)` with the usual `getDisabledReason` tooltip for
   non-admins.
2. **Edit dialog** — modeled on the existing create dialog:
   - Username shown read-only (the API does not support renaming).
   - Role selection: at minimum an "Administrator" toggle
     (promote/demote); ideally checkboxes for the three roles
     (`admin`, `viewer`, `connector`) so the dialog is the single place to
     manage roles. Keep the create dialog's semantics consistent if we go
     multi-role.
   - Submit via the existing `useUpdateUser` mutation (`PUT /users/:uid` with
     `{ roles: [...] }`), invalidate the `users` query, `toast` on
     success/failure.
3. **Guardrails in the dialog**:
   - Warn (confirmation step) when an admin demotes **themselves** — they lose
     access to the page they're on.
   - Disable demotion of the **last remaining admin**, with an explanatory
     tooltip.
4. **Backend guardrail (small)** — `handleUpdateUser` currently lets an admin
   remove the `admin` role from the last admin, bricking the instance.
   Add a server-side check rejecting a roles update that would leave zero
   admins (mirrors the protection presumably needed on `handleDeleteUser` —
   verify that path too while there).
5. **Test IDs + E2E** — `data-testid` on the edit button
   (`edit-user-<username>`), dialog, role toggles and submit; extend
   `front/e2e/users.spec.ts` with a promote → verify badge → demote flow using
   the test-mode users (`viewer`/`connector`).

### Open questions

- Should the edit dialog also expose `rate_limit_exempt` (already a column in
  the table)? Check whether `UpdateUserRequest` supports it; include it if
  cheap.
- Multi-role checkboxes vs. a single admin toggle: the description only asks
  for promote/demote, so the toggle is the minimum; decide when implementing.

No GitHub issue filed yet — one should be created when picking this up.
