# Query detail breadcrumb shows generic "Details" instead of "Queries › SELECT …"

## Problem

On a query detail page (e.g.
`https://dbbat.tools.stonal.io/app/queries/019ee46b-dab7-74ac-aa2a-0dfd79dc0884`)
the header breadcrumb at the top of the page reads just **"Details"**. It should
instead read **"Queries › SELECT …"** — a link back to the queries list, followed
by the first few characters of the actual SQL so the user can tell which query
they're looking at without scrolling.

Root cause: the breadcrumb is built entirely from route matches in
[Header.tsx:18-62](front/src/components/layout/Header.tsx#L18). For a detail
route it reads `match.context.title`, and when that's absent it falls back to
`formatPathname`, which hardcodes any UUID last segment to the literal string
`"Details"` ([Header.tsx:70-77](front/src/components/layout/Header.tsx#L70)).

Two gaps:
1. The `/queries` segment never produces its own crumb (there is no
   `queries.tsx` layout route, so no match with that pathname), so the "Queries"
   parent crumb is missing.
2. The query detail route
   ([queries/$uid.tsx:26-28](front/src/routes/_authenticated/queries/$uid.tsx#L26))
   sets no `title`, and the SQL text is only fetched client-side via
   `useQueryDetails` (TanStack Query), so it isn't available to the router
   context at breadcrumb-build time.

## Proposal

Make the query detail breadcrumb read `Queries › <sql-preview>`, where the
first crumb links to `/queries` and the second shows a truncated preview of the
query text.

- **Preview length**: show the first ~40 characters of the SQL, collapsing
  whitespace/newlines to single spaces and appending `…` when truncated
  (e.g. `SELECT id, name, created_at FROM users WHE…`). 40 keeps it readable in
  the header; 20 felt too short in the mockup. Make the constant easy to tweak.

- **Getting the SQL into the crumb** — pick one:
  - **(A, preferred) Make the breadcrumb query-aware.** Have the detail page
    publish the loaded query's SQL preview so the `Header` can render it — e.g.
    via a small context/store the page sets from `useQueryDetails`, or by
    letting the `Header` read the same cached query by uid. Keeps SQL fetching
    client-side (no route-loader change) and shows the real text once loaded,
    with a graceful "Queries › Details" (or a skeleton) while loading.
  - **(B) Route context title.** Set `context.title` on the route so
    `Header` picks it up generically — but the SQL isn't known at
    `beforeLoad`/context time, so this needs a route loader that fetches the
    query, duplicating the data fetch. Heavier; only worth it if we want the
    document `<title>` set too.

- **Ensure the "Queries" parent crumb appears.** Since there's no `/queries`
  layout match, either add the parent crumb explicitly for detail routes, or
  special-case the queries detail route to prepend a `{ title: "Queries",
  href: "/queries" }` crumb. Confirm the other detail pages (connections,
  databases, grants, users, audit) — they likely have the same missing-parent +
  "Details" issue and could share the fix.

- **Also fix `formatPathname`** so a bare UUID no longer collapses to the
  literal "Details" — that fallback is what makes every detail page look
  identical in the breadcrumb.

Note: the in-page `PageHeader` still shows "Query Details" / "Executed …"
([queries/$uid.tsx:98-99](front/src/routes/_authenticated/queries/$uid.tsx#L98));
this spec is about the top breadcrumb only, but consider whether the SQL preview
belongs in the `PageHeader` title too.

No originating GitHub issue yet — file one if this is picked up.

## Implementation Plan

Approach **A** (breadcrumb query-aware, no route-loader change), plus a generic
Header rewrite so the missing-parent crumb is fixed for every detail route.

1. **`src/lib/sql.ts`** — add `SQL_PREVIEW_MAX_LENGTH = 40` (easy to tweak) and
   `sqlPreview(sql)`: collapse all whitespace/newlines to single spaces, trim,
   truncate to the max length and append `…` when truncated.

2. **`src/contexts/BreadcrumbContext.tsx`** — a small provider holding a
   `titles: Record<pathname, string>` override map, with `setBreadcrumbTitle`
   and a convenience `useBreadcrumbTitle(path, title)` hook that publishes on
   mount and clears on unmount. Lets any page override the crumb text for its
   own path without touching the router loader.

3. **`src/routes/_authenticated.tsx`** — wrap the layout in `BreadcrumbProvider`
   so both `Header` and the page `Outlet` share it.

4. **`src/components/layout/Header.tsx`** — rebuild breadcrumbs from the deepest
   match's pathname split into cumulative segments (so `/queries` always yields
   a "Queries" parent crumb, fixing gap #1 for all detail pages). Each crumb
   title comes from the override map if set, else `formatSegment`. Fix
   `formatSegment` so a bare UUID no longer collapses to the literal "Details"
   — fall back to a short id (first 8 chars) so pages are distinguishable.

5. **`src/routes/_authenticated/queries/$uid.tsx`** — call
   `useBreadcrumbTitle('/queries/'+uid, query ? sqlPreview(query.sql_text) : undefined)`
   so the crumb shows `Queries › SELECT …` once loaded, and the plain path
   ("Queries › <short id>") while loading.

6. **QA** — `make build-front`, `bun run lint`; extend
   `front/e2e/observability.spec.ts` to assert the query-detail breadcrumb shows
   "Queries" + an SQL preview.
