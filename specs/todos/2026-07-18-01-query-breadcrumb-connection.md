---
model: sonnet
effort: medium
---

# The query detail breadcrumb never shows which connection the query belongs to

## Problem

On the query detail page (`/queries/$uid`), the breadcrumb reads
`Queries › SELECT …` — the SQL preview published via `useBreadcrumbTitle`
([front/src/routes/_authenticated/queries/$uid.tsx:38](../../front/src/routes/_authenticated/queries/$uid.tsx)).
The connection the query was executed on is nowhere in the breadcrumb — and in
fact nowhere on the page at all: the "Query Information" card shows status,
duration, rows affected and error, but not the connection.

Yet the connection is the natural parent of a query: the API's `Query` schema
carries a required `connection_id`
([internal/api/openapi.yml:2967](../../internal/api/openapi.yml)), and the
queries list already supports drilling down per connection via
`/queries?connection_id=<uid>`
([front/src/routes/_authenticated/queries/index.tsx:29](../../front/src/routes/_authenticated/queries/index.tsx)).
When showing a query, the current connection should always appear in the
breadcrumb, e.g.:

```
Queries › Connection 3f2a9c1e › SELECT * FROM users …
```

Two structural constraints explain why this isn't a one-liner:

- `Header.tsx` builds crumbs purely from cumulative URL path segments of the
  deepest route match
  ([front/src/components/layout/Header.tsx:27-45](../../front/src/components/layout/Header.tsx)).
  A connection crumb is not a path segment of `/queries/$uid`, so there is no
  slot to put it in today.
- `BreadcrumbContext` only supports overriding the *title* of an existing
  path-derived crumb (`useBreadcrumbTitle(pathname, title)`,
  [front/src/contexts/BreadcrumbContext.tsx:73](../../front/src/contexts/BreadcrumbContext.tsx));
  it cannot inject additional crumb items.

## Proposal

1. **Extend `BreadcrumbContext` to let a page inject extra crumbs.** Add a
   hook alongside `useBreadcrumbTitle`, e.g.
   `useBreadcrumbItems(pathname, items)` where `items` is
   `{ title: string; href?: string }[]`, stored per-pathname like titles are
   today. `Header.tsx` inserts those items *before* the leaf crumb for that
   pathname (i.e. between `Queries` and the SQL preview). Keep the existing
   title mechanism unchanged for other pages.

2. **Publish the connection crumb from the query detail page.** In
   `queries/$uid.tsx`, once `query` is loaded, publish
   `{ title: "Connection <first-8-of-uid>", href: "/queries?connection_id=<uid>" }`.
   The href reuses the existing filtered queries list (there is no connection
   detail page), which is also where the list page links connections from
   ([front/src/routes/_authenticated/queries/index.tsx:105](../../front/src/routes/_authenticated/queries/index.tsx)
   renders `connection_id.slice(0, 8)` — keep the same label convention).
   Since `connection_id` is required on the `Query` schema, the crumb appears
   whenever the query itself has loaded — "always", as requested.

3. **Tests.** Extend the E2E navigation/observability specs to assert the
   connection crumb is present on a query detail page and that clicking it
   lands on the queries list filtered by that connection.

Open question (don't block on it): a friendlier label than the short UID —
e.g. `db-name / username` — would require resolving the connection's
`database_id`/`user_id` with extra fetches (the query detail response only
populates them in list responses, per the OpenAPI description at
[internal/api/openapi.yml:2971-2984](../../internal/api/openapi.yml)). Ship the
short-UID crumb first; a follow-up can enrich the label.

## Implementation Plan

1. **`front/src/contexts/BreadcrumbContext.tsx`**: add an `items: Record<string, BreadcrumbExtraItem[]>`
   slot next to `titles`, plus `setBreadcrumbItems(pathname, items)` and a
   `useBreadcrumbItems(pathname, items)` hook mirroring `useBreadcrumbTitle`
   (publish on mount/update, clear on unmount, stable no-op when unchanged —
   compare by shallow value, not reference, to avoid an infinite effect loop
   from callers passing a fresh array each render).
2. **`front/src/components/layout/Header.tsx`**: read `items` from
   `useBreadcrumbContext()`. While building `breadcrumbs` from URL segments,
   right before pushing the leaf crumb for `pathname` (the last segment),
   splice in any extra items published for that exact pathname so they land
   between the parent segment crumbs and the leaf.
3. **`front/src/routes/_authenticated/queries/$uid.tsx`**: once `query` is
   loaded, call `useBreadcrumbItems(`/queries/${uid}`, ...)` with a single
   item `{ title: "Connection " + query.connection_id.slice(0, 8), href:
   "/queries?connection_id=" + query.connection_id }`, matching the label
   convention at `queries/index.tsx:105`. Pass `undefined`/`[]` while loading
   so no stale crumb flashes.
4. **Tests**: extend `front/e2e/observability.spec.ts` — add a case that
   opens a query detail page and asserts a "Connection <8-hex>" crumb is
   visible between "Queries" and the SQL-preview leaf, then click it and
   assert navigation to `/queries?connection_id=...` with the connection
   filter badge showing (reusing the pattern already in "queries list shows
   user, database, and connection columns").
