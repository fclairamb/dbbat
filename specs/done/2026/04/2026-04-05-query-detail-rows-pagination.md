# Query Detail: Result Rows Pagination

## Problem

The query detail page (`/app/queries/:uid`) fetches result rows with a default limit (100) and shows "Showing 100 of 923 rows" but provides no way to navigate to the remaining rows. For queries with hundreds or thousands of captured rows, the user can only see the first page.

## Current State

- `useQueryRows(uid)` fetches with default limit (100), no cursor
- The API supports cursor-based pagination (`next_cursor`, `has_more`, `total_rows`)
- The page shows a static message "Showing X of Y rows" when `has_more` is true
- No pagination controls

## Goals

- Navigate through all result rows with page controls
- Show current position ("Rows 101-200 of 923")
- Keep the URL clean — pagination state can be local (rows are a sub-section of the page, not the primary content)
- Configurable page size

## Design

### Pagination State

Unlike the queries list page (where pagination is URL-driven for bookmarkability), result rows pagination should be **component-local state** since:
- Users don't bookmark or share a specific page of result rows
- The rows table is a subsection of the query detail page
- URL params would clutter the query detail URL

```tsx
const [cursor, setCursor] = useState<string | undefined>();
const [pageSize, setPageSize] = useState(100);
const { data: rowsData } = useQueryRows(uid, { cursor, limit: pageSize });
```

### Pagination Controls

Below the rows table:

```
Rows per page: [50] [100] [500]          Rows 1-100 of 923    [← Previous] [Next →]
```

- **Page size selector**: 50 / 100 / 500
- **Position indicator**: "Rows X-Y of Z" using `row_number` from first and last row + `total_rows`
- **Previous / Next buttons**: Next uses `next_cursor` from the response. Previous requires tracking cursor history (a stack of previous cursors).

### Cursor History for "Previous"

The backend cursor is forward-only (`next_cursor`). To support "Previous", maintain a stack:

```tsx
const [cursorStack, setCursorStack] = useState<string[]>([]);
const [cursor, setCursor] = useState<string | undefined>();

const goNext = () => {
  if (rowsData?.next_cursor) {
    setCursorStack(prev => [...prev, cursor ?? ""]);
    setCursor(rowsData.next_cursor);
  }
};

const goPrevious = () => {
  setCursorStack(prev => {
    const next = [...prev];
    const prevCursor = next.pop();
    setCursor(prevCursor || undefined);
    return next;
  });
};

const goFirst = () => {
  setCursorStack([]);
  setCursor(undefined);
};
```

### Reset on Page Size Change

When the user changes the page size, reset to the first page:
```tsx
const changePageSize = (newSize: number) => {
  setPageSize(newSize);
  setCursor(undefined);
  setCursorStack([]);
};
```

## Files to Modify

- `front/src/routes/_authenticated/queries/$uid.tsx` — Add pagination state, controls, and cursor management to the Result Rows section

## Non-Goals

- URL-based pagination for rows (overkill for a sub-section)
- Infinite scroll (breaks the "page of data" mental model)
- Changing the backend pagination API (cursor-based already works well)
- Sorting or filtering rows (separate feature)
