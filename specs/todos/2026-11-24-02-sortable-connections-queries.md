---
model: sonnet
effort: medium
pr: own
automerge: false
---

# The Connections and Queries lists can only be read newest first

## Problem

Both lists are always ordered by uid, newest first: `q.Order("uid DESC")` in
`buildListConnectionsQuery` (`internal/store/connections.go:1041`) and
`q.Order("q.uid DESC")` in `buildListQueriesQuery` (`internal/store/queries.go:895`).
There is no way to find the longest sessions, the ones that moved the most data, or the
slowest statements, other than scrolling. The column titles (Connections: User, Database,
Grant, Source IP, Connected, Status, Duration, Queries, Data, Upstream; Queries: Query, User,
Database, Connection, Executed, Duration, Rows, Status) are plain text.

Sorting has to happen in the database, not on the fetched page: the same reasoning as
`ActiveOnly` (`connections.go`, comment above `filter.ActiveOnly`), sorting the newest
`limit` rows in the browser answers the wrong question.

## Proposal

1. API: `GET /api/v1/connections` and `GET /api/v1/queries` take `sort=<key>` and
   `order=asc|desc` (handlers in `internal/api/observability.go:172` and `:466`, next to the
   existing `limit`/`offset` parsing at :211/:219 and :515/:523). Keys are an allowlist,
   mapped to SQL in the store, never interpolated from the request. An unknown key or order
   is a `400`. Document both in `internal/api/openapi.yml` and regenerate the front types.
   - Connections: `connected_at` (default, maps to today's `uid` order), `duration`
     (`COALESCE(disconnected_at, now()) - connected_at`), `queries`, `bytes_transferred`.
   - Queries: `executed_at` (default, maps to `q.uid`), `duration_ms`, `rows_affected`.
2. Store: `ConnectionFilter` and `QueryFilter` get `Sort`/`Order` fields;
   `buildListConnectionsQuery` / `buildListQueriesQuery` apply them, always with `uid DESC`
   as tie-breaker so pages are stable. `NULL`s last whatever the direction (`duration_ms`
   of a statement still running).
3. Pagination: `before_uid` is a keyset on uid and only holds for the default sort. With a
   non-default sort the list pages with `offset`, which both endpoints already accept.
   Passing `before_uid` with a non-default `sort` is a `400`.
4. Front: the sortable column titles in `front/src/routes/_authenticated/connections/index.tsx`
   and in `front/src/components/shared/queryColumns.tsx` become buttons with an arrow
   showing the active sort. Click: sort desc; click again: asc; third click: back to the
   default. The sort lives in the route's search params (like the filters), so it survives a
   reload and can be linked. Changing the sort resets pagination to the first page.
   `data-testid="sort-<column>"` on each sortable title.
5. Auto-refresh ("Next: 1s") keeps the current sort.

## Tests

- `internal/store/connections_test.go`: each sort key orders as expected in both directions;
  equal values fall back to `uid DESC`; an open session sorts by its running duration.
- `internal/store/queries_test.go`: same for the query keys; a `NULL` `duration_ms` sorts
  last in both directions.
- Store: sort combines with every filter (user, server group, provenance, `active_only`,
  approval status) without changing which rows are selected, only their order.
- API tests in `internal/api/`: unknown `sort` → 400, `order=sideways` → 400, `before_uid`
  with a non-default sort → 400; a non-admin connector still sees only their own
  connections when sorting.
- E2E (`front/e2e/observability.spec.ts`): clicking "Duration" on each list orders the rows,
  a second click reverses, a third restores the default; reloading keeps the sort.

## To verify

- How each list paginates in the front today (`before_uid` "load more" or `offset`), in
  `front/src/api/queries.ts` (:1100, :1236, :1313 carry `offset`).
- The query-plan test that EXPLAINs `buildList*Query` (the comment above
  `buildListConnectionsQuery` mentions it; not found in
  `observability_filters_test.go`): extend it so a sort does not turn every listing into a
  full-table sort on the default path.
- Whether `queries` (about 408k rows on the largest known instance) needs an index on
  `duration_ms` / `rows_affected` for those sorts to stay fast with no filter.
  `connections` (about 61k rows) does not.

## Open questions

- **Sort by User and Database too?** They are ids in the row and names in the UI, so sorting
  by name needs a join on `users` / `servers`. Recommended: not in this spec; the filters
  already narrow by user and server, and the numeric columns are what sorting is for.
- **Sort Status?** Recommended: no; "Active only" and the approval filter already cover it.
- **Add indexes for the query sorts in this PR?** Recommended: only if the measurement under
  "To verify" shows an unfiltered sort above about 500 ms on a realistic volume.
