# Connections and Audit Logs: Links and Cursor Pagination

Follow-up to `2026-04-05-queries-page-links-pagination.md`. Apply the same pattern (proper links, UID-based cursor pagination, conditional auto-refresh) to the Connections and Audit Logs pages.

## Connections Page (`/app/connections`)

### Current State
- Uses `onRowClick` → `navigate({ to: "/queries", search: { connection_id: c.uid } })` — no real links
- Fetches `limit: 100` with no pagination
- Has an "Active only" toggle (client-side filter)

### Changes

**Links:** Replace `onRowClick` with `rowHref` pointing to `/queries?connection_id=<uid>`. This preserves the current behavior (clicking a connection shows its queries) but makes it a proper link.

**Backend:** Add `before` cursor param to `GET /connections` — same pattern as queries:
- `ConnectionFilter` gains `BeforeUID *uuid.UUID`
- `ListConnections` adds `WHERE uid < ?` clause, orders by `uid DESC`
- `handleListConnections` parses `before` query param

**Frontend:**
- URL params: `?before=<uid>&size=50`
- Page size selector (25 / 50 / 100)
- Newer / Older navigation links
- Auto-refresh only on first page (no `before` param)
- "Active only" toggle preserved (client-side filter, applied after fetch)

### Files to Modify
- `internal/store/models.go` — Add `BeforeUID` to `ConnectionFilter`
- `internal/store/connections.go` — Add `BeforeUID` handling to `ListConnections`
- `internal/api/observability.go` — Parse `before` in `handleListConnections`
- `front/src/routes/_authenticated/connections/index.tsx` — `rowHref`, pagination, conditional auto-refresh
- `front/src/api/queries.ts` — Add `before` param to `useConnections`

## Audit Logs Page (`/app/audit`)

### Current State
- No row click behavior (audit events are not linkable)
- Fetches `limit: 100` with no pagination
- Page title already says "Audit Log"

### Changes

**Links:** Audit events don't have a detail page, so no `rowHref` needed. No link changes.

**Backend:** Add `before` cursor param to `GET /audit`:
- `AuditFilter` gains `BeforeUID *uuid.UUID`
- `ListAuditEvents` adds `WHERE uid < ?` clause, orders by `uid DESC`
- `handleListAudit` parses `before` query param

**Frontend:**
- URL params: `?before=<uid>&size=50`
- Page size selector (25 / 50 / 100)
- Newer / Older navigation links
- Auto-refresh only on first page

### Files to Modify
- `internal/store/models.go` — Add `BeforeUID` to `AuditFilter`
- `internal/store/audit.go` — Add `BeforeUID` handling to `ListAuditEvents`
- `internal/api/observability.go` — Parse `before` in `handleListAudit`
- `front/src/routes/_authenticated/audit/index.tsx` — Pagination, conditional auto-refresh
- `front/src/api/queries.ts` — Add `before` param to `useAuditEvents`

## Implementation Notes

- The pagination UI (page size selector + Newer/Older buttons) is identical across all three pages. Consider extracting a `PaginationControls` component that takes `basePath`, `searchParams`, `size`, `firstUid`, `lastUid`, `hasMore` — but only if the duplication is bothersome after implementing. Don't over-abstract upfront.
- The backend `before` param works identically across all three endpoints since all use UUIDv7 primary keys.
