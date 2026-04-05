# Queries Page: Proper Links and Cursor Pagination

## Problem

The queries page (`/app/queries`) has two UX issues:

1. **No real links.** Rows use `onClick` + `navigate()` instead of `<a href>`. CMD+click, right-click "open in new tab", and browser link preview don't work. This breaks standard web navigation expectations.

2. **No pagination.** The page fetches `limit: 100` queries and stops. There's no way to see older queries or control page size. For a proxy that logs every query from every connection, 100 is quickly insufficient.

## Goals

- Query rows are standard `<a>` links supporting CMD+click, middle-click, right-click context menu
- UID-based cursor pagination (not offset-based, since queries are append-only with UUIDv7 ordering)
- Configurable page size
- Auto-refresh continues working, but only on the first page (latest queries)

## Design

### 1. Standard Links in DataTable

The `DataTable` component currently uses `onRowClick` with an `onClick` handler on `<tr>`. Replace this with a `rowHref` prop that wraps each row in an `<a>` tag (or makes the row act as a link).

Two approaches:

**Option A: Wrap the entire row in an `<a>` tag.** Semantically awkward (block link around table row), but works. TanStack Router's `<Link>` component handles CMD+click natively.

**Option B: Add a link in the first column cell, use CSS to stretch it over the full row.** This is the approach used by GitHub and many data-heavy apps. The first column cell contains an `<a>` with `::after { content: ''; position: absolute; inset: 0; }` to make the entire row clickable, while still being a proper link.

**Recommendation: Option B** — keeps table semantics clean and is a proven pattern.

The `DataTable` component gains a new prop:

```tsx
interface DataTableProps<T> {
  // ... existing props
  rowHref?: (item: T) => string;  // Replaces onRowClick
}
```

When `rowHref` is provided, the first column cell renders its content inside a `<Link>` that stretches to cover the whole row via CSS.

### 2. Cursor Pagination

#### Backend

The current backend `GET /queries` already supports `limit` and `offset`. However, offset-based pagination is fragile when new data is being inserted (queries shift between pages).

Add UID-based cursor support to `GET /queries`:

```
GET /api/v1/queries?limit=50&before=<uid>
```

- `before=<uid>`: Return queries with UID less than the given UID (older queries). Since UIDs are UUIDv7 (time-ordered), this gives stable cursor pagination.
- When `before` is absent, return the latest queries.
- Response includes the UID of the last item, which the frontend uses as the cursor for the next page.

The `QueryFilter` struct gains a `BeforeUID` field:

```go
type QueryFilter struct {
    // ... existing fields
    BeforeUID *uuid.UUID // Cursor: return queries with UID < this value
}
```

The SQL becomes: `WHERE uid < $before_uid ORDER BY uid DESC LIMIT $limit`.

#### Frontend

The queries page URL encodes pagination state:

```
/app/queries                     → latest queries (page 1)
/app/queries?before=<uid>&size=50 → queries before <uid>
```

Search params validated by TanStack Router:

```tsx
validateSearch: (search) => ({
  connection_id: search.connection_id as string | undefined,
  before: search.before as string | undefined,
  size: search.size ? Number(search.size) : 50,
})
```

The page renders:
- A page size selector (25 / 50 / 100)
- "Newer" / "Older" navigation buttons (using `<Link>` so they're proper URLs)
- "Newer" links to the page with `before` set to the first item's UID of the current page (or removes `before` for the latest page)
- "Older" links to the page with `before` set to the last item's UID

### 3. Auto-Refresh Behavior

- **First page (no `before` param):** Auto-refresh enabled. New queries appear at the top.
- **Paginated pages (`before` is set):** Auto-refresh disabled. The user is browsing history and shouldn't be disrupted.

This is already natural: when `before` is present, the query set is stable (no new items will appear before a fixed UID).

## Implementation Order

1. **Backend: Add `before` cursor param** to `GET /queries` — small change to `QueryFilter` and `ListQueries`.
2. **DataTable: Replace `onRowClick` with `rowHref`** — add link support using CSS stretched link pattern.
3. **Queries page: Add pagination controls** — URL-based `before`/`size` params, "Newer"/"Older" links.
4. **Queries page: Conditional auto-refresh** — disable when `before` is set.

## Files to Modify

- `internal/store/queries.go` — Add `BeforeUID` to `QueryFilter`, update `ListQueries` SQL
- `internal/api/observability.go` — Parse `before` query param in `handleListQueries`
- `internal/api/openapi.yml` — Document `before` query param
- `front/src/components/shared/DataTable.tsx` — Add `rowHref` prop with stretched link pattern
- `front/src/routes/_authenticated/queries/index.tsx` — Add pagination URL params, size selector, nav buttons, conditional auto-refresh
- `front/src/api/queries.ts` — Pass `before` param to API

## Non-Goals

- Infinite scroll (adds complexity, breaks URL-based navigation)
- Server-side total count (expensive for large datasets, not needed with cursor pagination)
- Changing other pages (connections, audit) — can be done later using the same pattern
