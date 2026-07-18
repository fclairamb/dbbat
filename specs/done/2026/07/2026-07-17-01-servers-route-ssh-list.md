---
model: sonnet
effort: medium
---

# The UI has no way to list SSH servers; the "Databases" page should become `/servers` with SSH bastions listed separately

## Problem

SSH bastion servers can be *created* from the Databases page — the add dialog has a
protocol option "SSH Bastion" (`front/src/routes/_authenticated/databases/index.tsx:356`)
— but they never appear anywhere afterwards. The `GET /databases` list deliberately
excludes SSH servers, and the dedicated `GET /ssh-servers` endpoint
(`front/src/api/queries.ts:288-297`, `useSSHServers`) is only consumed by the
"Via SSH server" selector inside the create-database dialog. Once created, an SSH
server cannot be seen, edited, or deleted from the UI.

On top of that, the page is named and routed as `/databases`
(`front/src/components/layout/AppSidebar.tsx:60`), which no longer matches its
content now that it also manages SSH bastions.

## Proposal

1. **Rename the route `/databases` → `/servers`**:
   - Move `front/src/routes/_authenticated/databases/index.tsx` to
     `front/src/routes/_authenticated/servers/index.tsx` (TanStack Router
     file-based routing).
   - Update the sidebar entry (`AppSidebar.tsx:60`) — title "Servers" (or keep
     "Databases" as the label? see open questions), href `/servers`.
   - Add a redirect from `/databases` to `/servers` so old links/bookmarks keep
     working.

2. **List SSH servers separately on the page**:
   - Keep the existing databases table as-is (still fed by `useDatabases()`).
   - Below it (or as a second section/tab), render an "SSH servers" list fed by
     `useSSHServers()`: name, host:port, username, and which databases tunnel
     through it if cheaply available; wire up the existing edit/delete mutations
     (or add them if missing for SSH servers).
   - The section only shows for roles allowed to manage servers (same visibility
     rules as the databases list/create button).

3. **Tests**: update any Playwright E2E tests referencing `/databases`, and add
   coverage that a created SSH bastion appears in the new SSH servers section.

## Open questions

- Sidebar label: "Servers" vs keeping "Databases"? Description says routing
  `/servers`, so "Servers" seems intended, but confirm.
- Should the SSH section be a tab, a separate card/table on the same page, or a
  sub-route (`/servers` with a filter)? Default: two stacked sections on one page
  (databases first, SSH servers second) — simplest and matches "separated".

## Implementation Plan

Backend check (done, no change needed): `PUT /servers/{uid}` and
`DELETE /servers/{uid}` (`internal/api/servers.go` `handleUpdateDatabase` /
`handleDeleteDatabase`) already operate generically on any `store.Server` row by
UID — `store.UpdateServer` / `store.DeleteServer` / `store.GetServerByUID` in
`internal/store/servers.go` do not filter by protocol. So SSH bastion rows can
already be edited/deleted through the existing endpoints; only the frontend needs
wiring. `GET /ssh-servers` is admin-only (`requireAdmin`), matching the existing
create/delete-database admin gating, so the new SSH section reuses the same
`admin`-only visibility check.

1. **Move the route**: `git mv front/src/routes/_authenticated/databases/index.tsx
   front/src/routes/_authenticated/servers/index.tsx`, update
   `createFileRoute("/_authenticated/servers/")`, rename the page component to
   `ServersPage`, title "Servers", keep the databases table/create-dialog/delete-dialog
   logic unchanged.
2. **Add an SSH servers section** to the same page, below the databases table:
   a stacked `DataTable` fed by `useSSHServers()` (name, host:port, username,
   actions), gated behind `canCreateDatabase(user?.roles)` (admin-only, matches
   `GET /ssh-servers`'s `requireAdmin`). Reuse `useDeleteDatabase` for the delete
   action (works because delete is UID-generic) and a lightweight confirmation
   dialog matching `DeleteDatabaseDialog`.
3. **Invalidate `["ssh-servers"]`** alongside `["databases"]` in
   `useCreateDatabase` / `useDeleteDatabase` (`front/src/api/queries.ts`) so the
   new section refreshes when a bastion is created/deleted from either dialog.
4. **Add a redirect**: new thin
   `front/src/routes/_authenticated/databases/index.tsx` that redirects to
   `/servers` in `beforeLoad` (pattern already used in
   `front/src/routes/_authenticated.tsx:14`), so old links/bookmarks keep working.
5. **Sidebar**: update `front/src/components/layout/AppSidebar.tsx:60` — title
   "Servers", href `/servers`.
6. **Tests**:
   - Rename `front/e2e/databases.spec.ts` → `front/e2e/servers.spec.ts`, update
     `goto("databases")` → `goto("servers")` and URL assertions to `/servers`.
   - Update `front/e2e/navigation.spec.ts`'s databases-link assertion to
     "Servers" / `/app/servers`.
   - Add a test creating an SSH bastion via the create-database dialog and
     asserting it appears in the new SSH servers section on `/servers`.
   - Add a redirect test: visiting `/databases` lands on `/servers`.
7. **QA**: `bun run build` (front) and `bun run lint` (front); run only the new/
   modified Playwright spec file if E2E can be exercised locally.
