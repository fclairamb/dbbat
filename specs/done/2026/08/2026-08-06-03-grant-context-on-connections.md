---
model: sonnet
effort: high
---

# Associate each connection with the grant it ran under, and show it in the UI

## Problem

A session is authorized by exactly one grant, chosen at auth time — but that
choice is recorded nowhere. `store.Connection`
(`internal/store/models.go:291`) has no grant reference, so:

- The UI cannot answer "under which grant did this session run?" — the grant
  context of every connection, query, and capture is invisible and, once the
  grant set changes, unrecoverable.
- `mayApproveQuery` (`internal/api/approvals.go:310-334`) has to *re-resolve*
  the active grant at approval time via `GetActiveGrant`. If a newer grant was
  created for the same user/database while a query sat on hold, the
  approver-group check uses the **newer** grant's `ApproverGroupUIDs`, not
  those of the grant whose `approval_patterns` actually triggered the hold.
- Quota counters (`populateGrantCounters`,
  `internal/store/grants.go:176-212`) can only aggregate by
  `(user_id, database_id)` window, so overlapping grants each get charged for
  the same traffic.

## Proposal

### Schema

Migration: `ALTER TABLE connections ADD COLUMN grant_uid uuid REFERENCES
access_grants(uid)` (nullable — pre-existing rows and any future grantless
paths stay null). Add the field to `store.Connection`
(`internal/store/models.go:291`) with `json:"grant_uid"`.

### Stamping

`CreateConnection` (`internal/store/connections.go:26`) takes the grant UID
(or the whole `*store.Grant`) and stamps it. All four proxies hold the grant
at auth time right where the connection row is created:

- PostgreSQL: `internal/proxy/postgresql/auth.go:73`
- Oracle: `internal/proxy/oracle/session.go:823`
- MySQL: `internal/proxy/mysql/auth.go:201`
- MongoDB: `internal/proxy/mongodb/auth.go:103`

### API

- Connection list/detail responses include `grant_uid`, and the detail
  response embeds a small grant summary (controls, starts/expires, revoked,
  priority) so the UI doesn't need an extra fetch. Update
  `internal/api/openapi.yml`.

### UI

- Connection detail page: a "Grant" section/badge showing the grant's
  controls + validity, linking to the grant.
- Connections list: a compact grant indicator (e.g. `R/W`, `R/O` chip derived
  from the linked grant's controls).

### Consumers (fixes enabled by the stamp)

1. **Approval check**: `mayApproveQuery` resolves the query's connection →
   `grant_uid` → grant, and checks `ApproverGroupUIDs` on *that* grant.
   Fall back to today's `GetActiveGrant` path only when the stamp is null
   (legacy rows); keep the fail-closed admin-only behavior when the grant is
   gone.
2. **Quota attribution**: `populateGrantCounters` can join
   `queries → connections` on `grant_uid` to charge traffic to the grant that
   actually carried it, instead of the whole user/database window. Fall back
   to the window heuristic for null stamps.

### Lifecycle note

The stamp records a **pinned** relationship: a session is bound to exactly one
grant from auth to disconnect. The `LimitGuard` watchdog already terminates
the connection when that grant expires or is revoked
(`internal/proxy/shared/limits.go:148`), so `grant_uid` never needs updating
on a live row — a connection outliving its grant is a bug, not a state to
represent. This also makes the UI story coherent: an ended connection shows
the grant it ran under, and `ErrGrantExpired` teardown explains *why* it
ended.

### Tests

- Store/proxy: connection row carries the auth-time grant UID (per protocol
  where cheap; PostgreSQL at minimum).
- Approval: hold a query under grant A (approver group G1), create newer
  grant B (group G2) → G1 can still approve, G2 cannot.
- Quota: two overlapping grants, traffic under one → only that grant's
  counters move.
- Frontend e2e: connection detail shows the grant context.

## Notes

- Implement after `2026-08-06-02-multi-grant-selection-priority.md` so the
  stamp records the priority-based selection; the two migrations are
  independent, only the semantics benefit from the ordering.

No GitHub issue filed yet — one should be.

## Implementation Plan

1. **Migration** `20260806010000_connections_grant_uid.{up,down}.sql`:
   `ALTER TABLE connections ADD COLUMN IF NOT EXISTS grant_uid uuid REFERENCES
   access_grants(uid)` (nullable, no index — read back one row at a time or
   alongside an existing scan; no `ON DELETE` since grants are revoked, never
   deleted).

2. **Store**:
   - `store.Connection.GrantUID *uuid.UUID` (`bun:"grant_uid,type:uuid"
     json:"grant_uid"`), added to both explicit `ColumnExpr` lists in
     `GetConnectionByUID` / `ListConnections`.
   - New `store.WithGrantUID(uuid.UUID) ConnectionOption` alongside the
     existing `WithUpstreamTLS`, so `CreateConnection`'s call sites opt in
     without changing its signature.
   - `populateGrantCounters` (`internal/store/grants.go`): both the query-count
     and bytes-transferred aggregates switch their `WHERE` to `(grant_uid = ?
     OR (grant_uid IS NULL AND user_id = ? AND database_id = ?))` — a stamped
     connection is charged to its own grant only, never falls through to a
     same-user/database window even if another grant's window also covers it;
     an unstamped (legacy) connection keeps the pre-existing window heuristic.

3. **Stamping** (one line added to each proxy's existing `CreateConnection`
   call, using the `*store.Grant` each session already holds at that point):
   - PostgreSQL `internal/proxy/postgresql/session.go` (`Run`, after
     `authenticate()` has set `s.grant`).
   - Oracle `internal/proxy/oracle/session.go` (`run()`, step 7, after
     `authenticateClient` at step 5 has set `s.grant`).
   - MySQL `internal/proxy/mysql/session.go` (`recordConnection`, called from
     `Run` after the handshake's `OnAuthSuccess` set `s.grant`).
   - MongoDB `internal/proxy/mongodb/session.go` (`recordConnection`, called
     from `establishSession` which received `grant` as a parameter).
   - All four sites are provably non-nil at the call (auth aborts the session
     before reaching `CreateConnection` otherwise); documented inline rather
     than adding a redundant nil check.

4. **API**:
   - `openapi.yml`: `Connection.grant_uid` (nullable uuid); a new
     `GrantSummary` schema (uid, controls, starts_at, expires_at, revoked,
     priority); `ConnectionDetail` gains `grant: GrantSummary | null`.
   - `internal/api/observability.go`: `handleGetConnection` resolves
     `conn.GrantUID` (when set) via `store.GetGrantByUID` and attaches a
     `*GrantSummary`; resolution failure (grant gone) degrades to `null`
     rather than failing the request. List endpoint unchanged (bare
     `Connection` rows, `grant_uid` only — no extra fan-out per row).
   - Regenerate `front/src/api/schema.ts` via `bun run generate-client`.

5. **Consumers**:
   - `mayApproveQuery` (`internal/api/approvals.go`): new
     `resolveApprovalGrant` helper — looks up the query's connection, and if
     it carries a `grant_uid`, resolves *that* grant via `GetGrantByUID`
     (fail-closed: a lookup error here is not masked by falling back to
     `GetActiveGrant`). Only when the connection lookup fails or `grant_uid`
     is nil does it fall back to today's `GetActiveGrant(user, database)`.
   - `populateGrantCounters`: see Store above.

6. **UI**:
   - Connection detail (`front/src/routes/_authenticated/connections/$uid.tsx`):
     a "Grant" card showing controls (R/W or the restriction badges), validity
     window, revoked state and priority, or "no grant on record" for legacy
     rows — with a link to `/grants` (there is no per-grant detail route
     today, so this links to the list rather than a specific row).
   - Connections list (`front/src/routes/_authenticated/connections/index.tsx`):
     a compact R/W / R/O chip per row, derived by fetching `useGrants()` (all,
     unfiltered — already scoped server-side to the caller's own grants for
     non-admins, matching the connection-list scoping) and matching on
     `grant_uid`.

7. **Tests**:
   - Store: `CreateConnection` + `WithGrantUID` round-trips through
     `GetConnectionByUID`/`ListConnections`; `populateGrantCounters` split
     test — two overlapping grants, traffic stamped to one, only that grant's
     counters move.
   - API: `mayApproveQuery` — hold under grant A (group G1), create grant B
     (group G2) with a higher priority, confirm G1 still resolves it and G2
     cannot; `handleGetConnection` embeds the grant summary.
   - Frontend: author an e2e assertion that the connection detail page renders
     grant context; note if it isn't run locally to avoid disturbing the
     shared dev instance.
