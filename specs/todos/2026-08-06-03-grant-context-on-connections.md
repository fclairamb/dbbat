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
