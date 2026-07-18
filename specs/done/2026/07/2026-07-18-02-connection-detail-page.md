---
model: sonnet
effort: high
---

# Connections have no detail page, so the query breadcrumb's connection crumb has nowhere real to point

## Problem

The query detail breadcrumb now reads `Queries › Connection 3f2a9c1e › SELECT …`
([front/src/routes/_authenticated/queries/$uid.tsx:48](../../front/src/routes/_authenticated/queries/$uid.tsx)),
but the connection crumb is a workaround on three fronts:

- **No target.** There is no connection detail page anywhere in the app —
  `queries/$uid` is the *only* detail route; every other section is a flat
  list. The crumb therefore links to `/queries?connection_id=<uid>`, a
  filtered query list, not the connection itself.
- **Opaque label.** `Connection 3f2a9c1e` is a UID prefix. The connections
  list already resolves user UIDs to usernames
  ([front/src/routes/_authenticated/connections/index.tsx:55](../../front/src/routes/_authenticated/connections/index.tsx));
  the crumb could say something like `florent @ pg-prod` instead.
- **Broken trail.** Clicking the crumb lands on the filtered queries list,
  whose breadcrumb is just `Queries` — the connection context the user
  navigated by evaporates.

A connection is the session-level unit of an observability proxy (user,
database, source IP, duration, query count, bytes transferred): it deserves a
page, and having one dissolves all three issues at once.

Backend gap: the API only has `GET /connections` (list,
[internal/api/openapi.yml:1477](../../internal/api/openapi.yml)); there is no
`GET /connections/{uid}`.

## Proposal

1. **Backend: `GET /connections/{uid}`.** Returns the existing `Connection`
   schema. Mirror the list endpoint's authorization exactly: **connectors may
   only fetch their own connections** (return 404, not 403, for others' —
   don't leak existence). Add to `openapi.yml`, implement the handler, and
   regenerate the frontend client (`bun run generate-client`).

2. **Frontend: `/_authenticated/connections/$uid.tsx`.** A detail page with:
   - a metadata card: resolved username and database name (reuse the lookup
     approach from the connections list), source IP, connected/disconnected
     timestamps, duration, query count, bytes transferred;
   - the connection's queries: reuse the queries-list table/hooks with a fixed
     `connection_id` filter;
   - a breadcrumb title published via `useBreadcrumbTitle` with the resolved
     `user @ database` label instead of the UUID fallback.

3. **Wire it in:**
   - connections list rows link to the detail page;
   - the query detail page's connection crumb (`useBreadcrumbItems` in
     `queries/$uid.tsx`) points its `href` to `/connections/<uid>` and, once
     the connection is fetched, upgrades its label from the short UID to the
     resolved `user @ database` form (short UID stays as the loading
     fallback).

4. **E2E**: navigate query → connection crumb → connection page; assert the
   metadata card and that the page's own breadcrumb is
   `Connections › <label>`. Also assert a connector user cannot open another
   user's connection page.

Keep query URLs flat (`/queries/$uid`) — do **not** move to nested
`/connections/$x/queries/$y` URLs; existing deep links must keep working and
the flat query log remains the primary browsing surface.

## Implementation Plan

1. **openapi.yml**: add a `ConnectionUID` path parameter (mirrors `QueryUID`/
   `GrantUID`, `internal/api/openapi.yml` around line 2040). Add a new
   `/connections/{uid}` path with `get`/`operationId: getConnection`,
   `description` noting connectors can only fetch their own (mirrors the list
   endpoint's note), responses `200` (`Connection` schema), `400`, `401`,
   `404`, `429`, `500`. Deliberately **no `403`** — per spec, others'
   connections 404 instead of leaking existence via 403.

2. **internal/store/connections.go**: add `GetConnectionByUID(ctx, uid)
   (*Connection, error)`, following `GetQuery`'s `sql.ErrNoRows` →
   `ErrConnectionNotFound` idiom, casting `source_ip::text` like
   `ListConnections` does.

3. **internal/api/observability.go**: add `handleGetConnection`. Parse uid
   (400 on bad uid), fetch via store (404 if not found), then if
   `!currentUser.IsAdmin() && !currentUser.IsViewer() && conn.UserID !=
   currentUser.UID` → **404** (not 403 — unlike `handleGetGrant`, existence
   must not leak to connectors).

4. **internal/api/server.go**: register `authenticated.GET("/connections/:uid",
   s.handleGetConnection)` next to the list route (no extra middleware — same
   as list, the ownership check lives in the handler).

5. Run `bun run generate-client` to refresh `front/src/api/schema.ts`.

6. **front/src/api/queries.ts**: add `useConnection(uid)` hook mirroring
   `useQueryDetails`.

7. **front/src/routes/_authenticated/connections/$uid.tsx**: new detail
   route. Metadata card (resolved username/database via `useUsers`/
   `useDatabases`, source IP, connected/disconnected timestamps, duration,
   query count, bytes transferred — reuse formatting helpers from
   `connections/index.tsx`), an embedded queries table for the connection
   (`useQueries({ connection_id: uid })`, reusing column defs from
   `queries/index.tsx`), and `useBreadcrumbTitle` publishing the resolved
   `user @ database` label. Gate on `canViewQueries`-equivalent permission
   (connections are visible to connector/viewer/admin — reuse existing
   connections-list permission, not the queries-only one). Handle
   not-found/403-as-404 the same way `queries/$uid.tsx` handles "Query not
   found".

8. **front/src/routes/_authenticated/connections/index.tsx**: change
   `rowHref` (line 177) from `` `/queries?connection_id=${c.uid}` `` to
   `` `/connections/${c.uid}` ``.

9. **front/src/routes/_authenticated/queries/$uid.tsx**: update the
   `useBreadcrumbItems` call (lines 48–58) to point `href` at
   `` `/connections/${query.connection_id}` `` instead of the filtered
   queries list, and upgrade the crumb label from the short UID to
   `username @ database` once `useConnection(query.connection_id)` resolves
   (short UID stays as the loading fallback, resolved via `useUsers`/
   `useDatabases` the same way the connections list does).

10. **E2E**: extend `front/e2e/observability.spec.ts` with a test that
    navigates query → connection crumb → connection detail page, asserts the
    metadata card and that the page's own breadcrumb reads `Connections ›
    <label>`, and asserts a connector user gets a not-found result for
    another user's connection (404, not a 403 page).

11. **QA**: `make lint`, Go tests scoped to `internal/api` and
    `internal/store` (or `make test` if scoping is awkward), `bun run lint`,
    `bun run build`, and the new/relevant Playwright spec.
