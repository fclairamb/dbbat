---
model: sonnet
effort: medium
---

# The "Recent Queries" table on the home page (`/`) should match the `/queries` table

## Problem

The dashboard/home page (`/`) and the dedicated Queries page (`/queries`) both
render a table of recent queries, but they render them **differently** — the two
column sets have drifted apart:

- Home — `front/src/routes/_authenticated/index.tsx:35-74` — 4 columns:
  - **Query** (SQL truncated to 60 chars, single line)
  - **Executed**
  - **Duration**
  - **Status**

- Queries — `front/src/routes/_authenticated/queries/index.tsx:66-142` — 8 columns:
  - **Query** (full SQL, `line-clamp-2`, `break-all`)
  - **User** (resolved via `getUserName`)
  - **Database** (resolved via `getDbName`)
  - **Connection** (link that filters `/queries` by `connection_id`)
  - **Executed**
  - **Duration**
  - **Rows** (`rows_affected`)
  - **Status**

The result is that the same data looks inconsistent depending on where you view
it — the home page hides User, Database, Connection and Rows, and renders the SQL
and Query cell with different styling. The user wants the home-page queries view
to be the **same** as the `/queries` one.

Note: the home page already fetches everything the richer columns need — it calls
`useUsers()`, `useDatabases()` and `useConnections()` (`index.tsx:19-23`), so no
new data fetching is required to render the full column set.

## Proposal

Make the two views render **identical query rows** by sharing a single column
definition instead of duplicating it — sharing (not copy/paste) is the point, so
they can't drift again.

1. **Extract the column set.** Pull the `Column<Query>[]` definition out of
   `queries/index.tsx` into a shared, reusable factory — e.g.
   `front/src/components/shared/queryColumns.tsx` exporting something like
   `buildQueryColumns({ users, databases, size })` (the `size` is only needed for
   the Connection-filter `Link` search params — default it so the home page can
   omit it). This keeps the User/Database name resolution (`getUserName`,
   `getDbName`) and the Connection link in one place.

2. **Use it on both pages.**
   - `/queries` (`queries/index.tsx`) consumes the factory instead of its inline
     `columns` array — behaviour unchanged.
   - Home (`index.tsx`) replaces its reduced `queryColumns` with the same factory,
     passing the `users`/`databases` it already fetches. The "Recent Queries"
     `DataTable` then renders the full 8-column layout identically to `/queries`.

3. **Scope: columns/rendering only, not the page chrome.** The home page keeps its
   `limit: 10` fetch, its "Recent Queries" heading, and stays free of the
   `/queries`-only controls (pagination, `AdaptiveRefresh`, connection-filter
   badge). "The same" here means the **table/rows look the same**, not that the
   dashboard grows a full pager. Confirm this interpretation if unsure — the
   alternative (making `/` a full queries page) seems out of scope for a dashboard
   summary.

4. **Verify** with the running dev server (`admin`/`admintest`): the home-page
   Recent Queries table shows User, Database, Connection (clickable → filtered
   `/queries`), Rows, and the same SQL cell styling as `/queries`. Run
   `bun run typecheck` and `bun run lint` in `front/`.

## Notes

- No GitHub issue is linked yet — one should be filed for traceability
  (`specs/README.md` asks specs to reference their originating issue).
