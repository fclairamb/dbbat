---
model: opus
effort: high
---

# Grants get an explicit priority column, auto-calculated from controls by default

## Problem

When a user has more than one active grant on the same database, the proxy
picks one essentially at random — and everything downstream (controls, quotas,
approval patterns, read-only pinning) follows that single pick.

The only selection point is `Store.GetActiveGrant`
(`internal/store/grants.go:93-116`): filter on user/database/active window,
then `ORDER BY created_at DESC LIMIT 1`. **Newest created wins.** All four
proxies snapshot that one grant at auth time
(`internal/proxy/postgresql/auth.go:73`,
`internal/proxy/oracle/session.go:823`, `internal/proxy/mysql/auth.go:201`,
`internal/proxy/mongodb/auth.go:103`) and every validator takes exactly one
`*store.Grant` (`internal/proxy/shared/validation.go`, `limits.go:68`,
`approval.go:178`).

Overlap is trivially reachable: nothing prevents it at insert time — neither
`POST /grants` (`internal/api/grants.go:32-98`) nor request approval
(`internal/store/grant_requests.go:150-211`) checks existing active grants,
and the initial-schema comment claiming "Active grant uniqueness is enforced
at application level"
(`internal/migrations/sql/20260107000000_initial_schema.up.sql:141-143`) is
untrue — no such enforcement exists.

Concrete failure: a user auto-approves a `read-only-1h` grant on prod, then a
`readwrite-30m` one. The newer R/W grant wins, so the session is not pinned
read-only and the R/O grant's approval patterns and quotas are silently
ignored. Reverse the creation order and the user is silently *denied* writes
their R/W grant should allow. The outcome depends on creation order, which no
one controls deliberately.

## Decision (user-set)

Add an explicit **`priority` column** on grants. It is **auto-calculated by
default** from the grant's conditions — `R/W > R/W with controls > R/O` — and
**exposed as an editable field in the frontend**, so an admin can override the
computed default when needed. Selection becomes highest-priority-first.

## Proposal

### Schema

- Migration: `ALTER TABLE access_grants ADD COLUMN priority smallint NOT NULL
  DEFAULT 0`, plus a backfill `UPDATE` computing the auto value for existing
  rows (see formula below). Index not required — the active-grant partial
  index already narrows the candidate set.
- `store.AccessGrant` (`internal/store/models.go:468-498`): add
  `Priority int16`.

### Auto-calculation

Tiered, with gaps so manual overrides can slot between tiers:

| Grant shape | Auto priority |
|---|---|
| Writable, no controls (full R/W) | 100 |
| Writable with controls (`block_copy` / `block_ddl`) | 50 |
| `read_only` (with or without other controls) | 10 |

Implemented once in Go (e.g. `store.AutoPriority(controls []string) int16`)
and used by every creation path when no explicit priority is supplied:

- `POST /grants` (`internal/api/grants.go:32`): accept optional `priority` in
  the request body; when absent, compute.
- Definition materialization (`BuildGrantFromDefinition`,
  `internal/store/grants.go:17-38`): grant definitions get an optional
  priority too (same nullable-means-auto rule), copied to the grant.
- OpenAPI (`internal/api/openapi.yml`): document the field on grant create,
  grant responses, and grant definitions.

### Selection

`GetActiveGrant` (`internal/store/grants.go:93-116`) becomes:

```sql
ORDER BY priority DESC, expires_at DESC, created_at DESC
LIMIT 1
```

Mirror the same ordering in `ListGrants` (`internal/store/grants.go:139-171`)
so the UI lists grants in the order the proxy would pick them.

### Frontend

- Grant creation form: a `Priority` field, pre-filled live from the selected
  controls using the same tier formula; once the user edits it manually, stop
  syncing (standard dirty-field behavior). Show a hint like "auto: 50" so the
  override is a conscious act.
- Grant list & detail: display the priority value.
- Grant definitions form: same optional field.

### Invariant: a session lives and dies with its grant

Priority selection happens **once, at auth time**. The session stays pinned to
the grant it was admitted under for its entire life; there is no mid-session
failover to another still-active grant. When the pinned grant expires (or is
revoked), the connection must terminate — this already works today via the
`LimitGuard` watchdog, which snapshots the grant's `expires_at` and tears the
session down on `ErrGrantExpired` (`internal/proxy/shared/limits.go:148`,
250ms poll; wired in all four proxies:
`internal/proxy/postgresql/session.go:332`,
`internal/proxy/oracle/session.go:1159`,
`internal/proxy/mysql/session.go:199`,
`internal/proxy/mongodb/session.go:216`). The multi-grant change must not
weaken this: the client reconnects after termination and priority selection
runs afresh, picking the next-best still-active grant.

### Tests

- Store: overlapping R/O + R/W grants in both creation orders → R/W wins both
  times; explicit low priority on the R/W grant → R/O wins; tie on priority →
  latest expiry, then newest.
- Lifecycle: session admitted under grant A while grant B is also active; A
  expires → the session is terminated even though B still allows access
  (regression guard for the invariant above); a reconnect is admitted under B.
- API: create-grant with and without explicit priority; response echoes it.
- Frontend e2e: field auto-updates with controls, manual override sticks.

### Notes / open questions

- Companion spec `2026-08-06-03-grant-context-on-connections.md` stamps the
  chosen grant on each connection, which makes the selection auditable in the
  UI — implement that one after this, so the stamp reflects priority-based
  selection.
- Open question: should selection skip a quota-exhausted grant so it doesn't
  shadow a still-usable lower-priority one? Deferred — not part of this
  change unless trivially cheap.
- Fix the stale uniqueness comment in the initial-schema migration while
  touching the schema.

No GitHub issue filed yet — one should be.

## Implementation Plan

1. **Migration** `internal/migrations/sql/20260806000000_grants_priority.{up,down}.sql`
   - `ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS priority SMALLINT NOT NULL DEFAULT 0`
   - Backfill `UPDATE` computing the tier (100 / 50 / 10) from `controls` for every
     pre-existing row.
   - `ALTER TABLE grant_definitions ADD COLUMN IF NOT EXISTS priority SMALLINT` —
     nullable, NULL meaning "auto from controls at materialization time".
   - Corrective comment about the false "Active grant uniqueness is enforced at
     application level" note lives here (shipped migrations are immutable).
   - Down migration drops both columns.

2. **Store** (`internal/store/`)
   - `models.go`: `AccessGrant.Priority int16` (`bun:"priority,notnull,default:0"`),
     `GrantDefinition.Priority *int16` (`bun:"priority"`).
   - `grants.go`: `AutoPriority(controls []string) int16` — the single formula
     (read_only → 10, any other control → 50, none → 100).
   - `CreateGrant`: `Priority` = supplied value when non-zero-by-caller… actually
     the caller passes an already-resolved value; the API/definition paths resolve
     nil → `AutoPriority`. `CreateGrant` itself falls back to `AutoPriority` when
     the field is left at 0 so no path can insert an unintentional 0.
   - `BuildGrantFromDefinition`: `def.Priority` when non-nil, else `AutoPriority`.
   - `GetActiveGrant` + `ListGrants`: `ORDER BY priority DESC, expires_at DESC,
     created_at DESC`.
   - `UpdateGrantDefinition`: add `priority` to the updated column list.

3. **API** (`internal/api/`)
   - `CreateGrantRequest.Priority *int16` — absent → auto.
   - `CreateGrantDefinitionRequest.Priority *int16` — absent → auto at
     materialization.
   - Validate the range fits a smallint.
   - Audit event records the resulting priority.

4. **OpenAPI** (`internal/api/openapi.yml`): `priority` on `AccessGrant`,
   `CreateGrantRequest`, `GrantDefinition`, `CreateGrantDefinitionRequest`.
   Regenerate `front/src/api/schema.ts`.

5. **Frontend**
   - `grants/index.tsx`: Priority column in the list; Priority input in the create
     dialog, live-synced from the selected controls until manually edited
     ("auto: N" hint + a reset affordance).
   - `grant-definitions/index.tsx`: Priority column + the same optional
     auto-synced field in the definition dialog (blank = auto).

6. **Tests**
   - `internal/store/grants_test.go`: both creation orders, explicit override,
     tie-breaking on expiry then creation.
   - `internal/store/grants_test.go` (lifecycle): a `LimitGuard` built from the
     admitted grant still fires `ErrGrantExpired` while a second grant is active.
   - `internal/api/grants_test.go`: create with and without an explicit priority.
   - `front/e2e/grants.spec.ts`: auto-update on control toggle + manual override
     sticks.
