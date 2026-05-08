# Grant definitions

## Goal

Introduce a new admin-managed entity, **`grant_definitions`**, that describes
the *shape* of a grant: a name, a duration, a set of access controls, and
optional quotas. Definitions are the menu of pre-approved access shapes that
users will be able to request from in Spec 03 (`03-grant-requests.md`).

A definition does **not** target a specific user or database — it's a
template. The user + database come from the request that consumes it.

## Why

Today only admins create grants, one off, by filling 7 fields. Once we let
users self-serve via requests (Spec 03), we need a small set of
admin-controlled shapes ("Read-only 1h", "Investigator 4h", "Audit 30 min")
that bound what users can ask for. The definition is also the natural place
to encode the duration as a fixed offset (e.g. 3600 seconds) rather than two
absolute timestamps the user would have to pick.

Direct admin grant creation (`POST /api/v1/grants`) keeps working unchanged
and bypasses the definition system — admins know what they're doing.

## Schema

New migration:
`internal/migrations/sql/20260509000000_grant_definitions.up.sql`

```sql
CREATE TABLE grant_definitions (
    uid                   UUID PRIMARY KEY,
    name                  TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    duration_seconds      BIGINT NOT NULL CHECK (duration_seconds > 0),
    controls              TEXT[] NOT NULL DEFAULT '{}',
    max_query_counts      INTEGER,           -- NULL = unlimited
    max_bytes_transferred BIGINT,            -- NULL = unlimited
    is_active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_by            UUID NOT NULL REFERENCES users(uid),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX grant_definitions_active_name_uniq
    ON grant_definitions(name) WHERE is_active;
```

Down migration drops the table.

Notes:
- `controls` mirrors the `access_grants.controls` column (TEXT[] of
  `read_only`, `block_copy`, `block_ddl`).
- Soft-delete via `is_active=false` to preserve historical references from
  `grant_requests` and audit logs.
- Unique-active-name keeps the menu tidy without preventing reuse of an old
  name after deactivation.

## Files added / modified

**Add**
- `internal/migrations/sql/20260509000000_grant_definitions.up.sql`
- `internal/migrations/sql/20260509000000_grant_definitions.down.sql`
- `internal/store/grant_definitions.go`
- `internal/store/grant_definitions_test.go`
- `internal/api/grant_definitions.go`
- `internal/api/grant_definitions_test.go`
- `front/src/routes/_authenticated/grant-definitions/index.tsx`

**Modify**
- `internal/store/models.go` — append `GrantDefinition` struct.
- `internal/api/server.go` — register the new routes.
- `internal/api/openapi.yml` — add `GrantDefinition` schema and the five paths.
- `front/src/lib/permissions.ts` — add `canManageGrantDefinitions()`.
- `front/src/routes/_authenticated/_layout.tsx` (or wherever the sidebar nav
  lives) — add a "Grant Definitions" link visible only to admins.

## Backend

### Model

```go
// internal/store/models.go (append)
type GrantDefinition struct {
    bun.BaseModel `bun:"table:grant_definitions,alias:gd"`

    UID                 uuid.UUID `bun:"uid,pk,type:uuid"                      json:"uid"`
    Name                string    `bun:"name,notnull"                           json:"name"`
    Description         string    `bun:"description,notnull,default:''"         json:"description"`
    DurationSeconds     int64     `bun:"duration_seconds,notnull"               json:"duration_seconds"`
    Controls            []string  `bun:"controls,array,notnull,default:'{}'"    json:"controls"`
    MaxQueryCounts      *int      `bun:"max_query_counts"                        json:"max_query_counts,omitempty"`
    MaxBytesTransferred *int64    `bun:"max_bytes_transferred"                   json:"max_bytes_transferred,omitempty"`
    IsActive            bool      `bun:"is_active,notnull,default:true"         json:"is_active"`
    CreatedBy           uuid.UUID `bun:"created_by,notnull,type:uuid"           json:"created_by"`
    CreatedAt           time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}
```

Match the bun tag convention used by the existing `Grant` struct
(`internal/store/models.go:220-275`).

### Store

`internal/store/grant_definitions.go` — mirror the layout of
`internal/store/grants.go`:

```go
func (s *Store) CreateGrantDefinition(ctx context.Context, def *GrantDefinition) error
func (s *Store) GetGrantDefinition(ctx context.Context, uid uuid.UUID) (*GrantDefinition, error)
func (s *Store) ListGrantDefinitions(ctx context.Context, activeOnly bool) ([]*GrantDefinition, error)
func (s *Store) UpdateGrantDefinition(ctx context.Context, def *GrantDefinition) error
func (s *Store) DeactivateGrantDefinition(ctx context.Context, uid uuid.UUID) error
```

Validation in `Create`/`Update`:
- `name` non-empty, ≤ 64 chars
- `duration_seconds` > 0 and ≤ 30 days (configurable later if needed)
- each `controls` entry in the allowed set
- if `max_query_counts != nil`, value > 0; same for `max_bytes_transferred`

### API

Routes (registered in `internal/api/server.go`):

| Method | Path                                  | Auth         |
|--------|---------------------------------------|--------------|
| GET    | `/api/v1/grant-definitions`           | any user     |
| GET    | `/api/v1/grant-definitions/:uid`      | any user     |
| POST   | `/api/v1/grant-definitions`           | admin        |
| PATCH  | `/api/v1/grant-definitions/:uid`      | admin        |
| DELETE | `/api/v1/grant-definitions/:uid`      | admin (soft) |

`GET` for non-admin users automatically filters `is_active=true`. Admins see
both active and inactive (with an `?active=` query param to filter).

Use the existing `requireAdmin()` middleware
(`internal/api/middleware.go`) on the write routes.

Audit:
- `grant_definition.created`, `grant_definition.updated`,
  `grant_definition.deactivated` via `LogAuditEvent` in
  `internal/store/audit.go`. Mirror the pattern in `internal/api/grants.go`.

### OpenAPI

In `internal/api/openapi.yml`, add the `GrantDefinition` schema and the five
paths. Reuse the `controls` enum already defined for grants.

```yaml
GrantDefinition:
  type: object
  properties:
    uid: { type: string, format: uuid, readOnly: true }
    name: { type: string, maxLength: 64 }
    description: { type: string }
    duration_seconds: { type: integer, format: int64, minimum: 1 }
    controls:
      type: array
      items: { type: string, enum: [read_only, block_copy, block_ddl] }
    max_query_counts: { type: integer, nullable: true }
    max_bytes_transferred: { type: integer, format: int64, nullable: true }
    is_active: { type: boolean, readOnly: true }
    created_by: { type: string, format: uuid, readOnly: true }
    created_at: { type: string, format: date-time, readOnly: true }
  required: [name, duration_seconds]
```

## Frontend

`front/src/routes/_authenticated/grant-definitions/index.tsx` (new, admin-only):

- List view with columns: name, duration (humanized: "1 h", "30 min"),
  controls badges, quota summary, status (active/inactive), actions
  (edit / deactivate).
- "New definition" button → modal form. Reuse the controls and quotas form
  blocks from `front/src/routes/_authenticated/grants/index.tsx` so the
  visual language is consistent. Replace the date-pickers with a single
  duration picker (number + unit selector: minutes / hours / days).
- Edit modal mirrors the new-definition form, prefilled.
- Deactivate is a confirm dialog (no hard delete).

`front/src/lib/permissions.ts`:

```ts
export const canManageGrantDefinitions = (user: User | null): boolean =>
  user?.roles?.includes("admin") ?? false;
```

Sidebar entry visible only when `canManageGrantDefinitions()` returns true.

## Tests

### Backend
- Store: create / get / list (active-only and not) / update / deactivate /
  duplicate-active-name fails / duplicate after deactivate succeeds.
- API: admin can do everything; viewer can list active; connector can list
  active; PATCH/POST/DELETE forbidden for non-admin → 403.
- Audit: each write produces the expected audit event row.

### Frontend (e2e via Playwright)
- Admin sees the sidebar link; viewer/connector do not.
- Admin can create, edit, deactivate a definition; the list refreshes.

## Verification checklist

- [ ] `make lint` clean, `make test` green
- [ ] `./dbbat db migrate` applies the new migration; `./dbbat db rollback`
      reverses it
- [ ] Swagger UI at `/api/docs` lists the new paths
- [ ] Admin creates "Read-only 1h" via the UI: read_only, 1000 query cap,
      100 MB cap, 3600s — saved and editable
- [ ] Connector account sees the same definition via
      `GET /api/v1/grant-definitions` but cannot create one
- [ ] Audit log shows `grant_definition.created` for the action

## Out of scope

- Linking definitions to specific databases (a definition is database-agnostic;
  the request specifies the database).
- Per-role visibility filters on definitions (any user can see any active
  definition).
- Approval policies based on definition (Spec 03 has a single
  approve/deny by an admin).
