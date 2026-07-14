---
model: sonnet
effort: medium
---

# Global queries list should show user, database, and connection

Originating issue: [#253](https://github.com/fclairamb/dbbat/issues/253)

## Problem
On the global queries view (`/app/queries`,
`front/src/routes/_authenticated/queries/index.tsx`) — i.e. listing queries
without targeting a specific connection — the rows don't show which user ran the
query, against which database, or on which connection. That context only appears
when drilling into a single connection, making the global list hard to
interpret.

## Proposal
- Add columns to the global queries list showing the **user**, the **database**,
  and the **connection** each query belongs to.
- Resolve the user name with the same `getUserName(uid)` helper pattern used on
  the grants/connections pages
  (`front/src/routes/_authenticated/connections/index.tsx:54`).
- Backend: ensure the queries listing endpoint returns (or lets the frontend
  join) `user_id`, database id/name, and connection id for each query. If the
  payload lacks these, extend the query listing in `internal/api` and
  `internal/store` and update `internal/api/openapi.yml`.
- Link the connection cell through to the per-connection query view
  (`queries/$uid.tsx`) where it makes sense.

## Implementation Plan

### 1. Backend: store query joins `connections` for user/database

- `internal/store/models.go`: add `UserID *uuid.UUID` and `DatabaseID
  *uuid.UUID` fields to the `Query` struct, tagged `bun:"user_id,scanonly"` /
  `bun:"database_id,scanonly"` (`json:"user_id,omitempty"` /
  `json:"database_id,omitempty"`) so they're populated on scan but bun never
  tries to insert/update them (the `queries` table has no such columns).
- `internal/store/queries.go` `ListQueries`: always `JOIN connections c ON
  q.connection_id = c.uid` (currently only joined when filtering by
  user/database) and add `c.user_id, c.database_id` to the `ColumnExpr` so
  every listed query carries its connection's user and database.
- Leave `GetQuery` / `GetQueryWithRows` (single-query fetch) untouched —
  out of scope, and the detail page doesn't need these columns.

### 2. Backend: API contract

- `internal/api/openapi.yml` `Query` schema: add `user_id` and `database_id`
  as nullable `uuid` properties (not `required`, since only the list endpoint
  populates them).
- No handler code changes needed — `handleListQueries` already returns the
  `store.Query` structs as-is via `successResponse`.

### 3. Regenerate frontend client

- `cd front && bun run generate-client` to regenerate `src/api/schema.ts`
  from the updated OpenAPI spec. Commit the regenerated file.

### 4. Frontend: render the new columns

- `front/src/routes/_authenticated/queries/index.tsx`:
  - Fetch `useUsers()` and `useDatabases()` (mirrors
    `connections/index.tsx`), add local `getUserName(uid)` /
    `getDbName(uid)` helpers.
  - Add "User" and "Database" columns using those helpers, and a
    "Connection" column showing a short/truncated connection id.
  - The "Connection" cell links to `/queries?connection_id={uid}` (the
    existing per-connection queries view, same URL pattern already used by
    `connections/index.tsx`'s `rowHref`). Since `DataTable`'s row-level
    `rowHref` already covers the whole row with an absolutely-positioned
    overlay `Link` (via the first column), the nested connection Link needs
    `relative z-10` (or similar) classes so it sits above that overlay and
    remains independently clickable.

### 5. Tests

- `internal/store/queries_test.go`: extend `TestListQueries` (or add a new
  subtest) asserting `result[i].UserID` / `DatabaseID` are non-nil and match
  the connection's `UserID`/`DatabaseID` used to create the test queries.
- `front/e2e/observability.spec.ts`: extend the "should display queries
  page" test (or add a new one) asserting the table header includes "User",
  "Database", and "Connection" columns, and that the connection link
  navigates to `/queries?connection_id=...`.
