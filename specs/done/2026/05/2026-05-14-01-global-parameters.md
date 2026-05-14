# Global Parameters Table

## Status: Todo

## Summary

Ship the deferred half of the 2026-01-24 draft spec (`specs/done/2026/01/2026-01-24-global-parameters-user-identities.md`): a generic `global_parameters(group_key, key, value)` key-value store plus a typed wrapper for public-endpoint advertisement (the host + per-protocol port/host overrides that power connection-URL display). Includes an admin settings page for editing these values.

## Problem

dbbat proxies are running but users have no way to discover what host/port to connect to. There is also no runtime-editable configuration store — every setting is an env var requiring a restart.

The 2026-01-24 draft proposed a `global_parameters` table for this purpose; only the `user_identities` half was implemented. This spec completes the other half, using the exact schema and store-method shape from the draft so no design divergence occurs.

## Solution

### Backend

#### Migration

**File**: `internal/migrations/sql/20260514000000_global_parameters.up.sql`

Use `--bun:split` between statements.

```sql
CREATE TABLE global_parameters (
    uid       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_key TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

--bun:split

CREATE INDEX idx_global_parameters_group_key ON global_parameters(group_key);
CREATE UNIQUE INDEX idx_global_parameters_unique
    ON global_parameters(group_key, key) WHERE deleted_at IS NULL;
CREATE INDEX idx_global_parameters_deleted_at
    ON global_parameters(deleted_at) WHERE deleted_at IS NULL;
```

**File**: `internal/migrations/sql/20260514000000_global_parameters.down.sql`

```sql
DROP TABLE IF EXISTS global_parameters;
```

Add `global_parameters` to `Store.DropAllTables` in `internal/store/store.go` (no foreign keys so order is flexible).

#### Go model

Add `GlobalParameter` to `internal/store/models.go` after the `GrantDefinition` block, verbatim from the 2026-01-24 draft:

```go
type GlobalParameter struct {
    bun.BaseModel `bun:"table:global_parameters,alias:gp"`

    UID       uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
    GroupKey  string     `bun:"group_key,notnull" json:"group_key"`
    Key       string     `bun:"key,notnull" json:"key"`
    Value     string     `bun:"value,notnull" json:"value"`
    CreatedAt time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
    UpdatedAt time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
    DeletedAt *time.Time `bun:"deleted_at,soft_delete" json:"-"`
}
```

#### Store — `internal/store/global_parameters.go`

```go
var ErrParameterNotFound = errors.New("parameter not found")

// GetParameter retrieves a single active parameter by group and key.
func (s *Store) GetParameter(ctx context.Context, groupKey, key string) (*GlobalParameter, error)

// GetParameters retrieves all active parameters for a group.
func (s *Store) GetParameters(ctx context.Context, groupKey string) ([]GlobalParameter, error)

// SetParameter creates or updates a parameter (upsert on group_key+key).
func (s *Store) SetParameter(ctx context.Context, groupKey, key, value string) error

// DeleteParameter soft-deletes a parameter.
func (s *Store) DeleteParameter(ctx context.Context, groupKey, key string) error
```

Upsert via:
```sql
INSERT INTO global_parameters (group_key, key, value, updated_at)
VALUES (?, ?, ?, NOW())
ON CONFLICT (group_key, key) WHERE deleted_at IS NULL
DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
```

**Encryption note**: the `enc:` prefix is reserved for future encrypted values (per the 2026-01-24 draft). Store methods in this spec do NOT encrypt — all parameters in this PR are non-sensitive. Do not strip or interpret the prefix in the basic CRUD methods; leave that for a future `SetParameterEncrypted`/`GetParameterDecrypted` pair.

#### Typed public-endpoint wrapper — also in `internal/store/global_parameters.go`

The `public.*` parameter group is used by the connection-URL feature (spec 2). It lives here to keep key constants co-located with the store.

```go
const (
    GroupPublic        = "public"
    KeyPublicHost      = "host"
    KeyPublicPGHost    = "pg.host"
    KeyPublicOraHost   = "ora.host"
    KeyPublicMySQLHost = "mysql.host"
    KeyPublicPGPort    = "pg.port"
    KeyPublicOraPort   = "ora.port"
    KeyPublicMySQLPort = "mysql.port"
)

type PublicEndpoints struct {
    Host      string // default public hostname for all protocols
    PGHost    string // optional override; "" = fall back to Host
    OraHost   string
    MySQLHost string
    PGPort    *int   // optional override; nil = fall back to local listen port
    OraPort   *int
    MySQLPort *int
}

// GetPublicEndpoints reads all public.* parameters and returns the typed struct.
// Missing keys default to zero/nil (caller should use ResolvePublicEndpoints for fallback).
func (s *Store) GetPublicEndpoints(ctx context.Context) (PublicEndpoints, error)

// SetPublicEndpoints writes only the non-empty/non-nil fields (zero/nil = "no override").
func (s *Store) SetPublicEndpoints(ctx context.Context, pe PublicEndpoints) error
```

```go
type ResolvedEndpoints struct {
    PGHost    string
    OraHost   string
    MySQLHost string
    PGPort    int // 0 = protocol disabled (empty listen addr)
    OraPort   int
    MySQLPort int
}

// ResolvePublicEndpoints applies fallback chains:
//   host:  protocol-specific override → pe.Host → ""
//   port:  protocol-specific override → local listen port parsed from cfg → 0
// Local ports parsed from cfg.ListenPG / cfg.ListenOracle / cfg.ListenMySQL via net.SplitHostPort.
// An empty listen string (protocol disabled) resolves to port 0.
func ResolvePublicEndpoints(pe PublicEndpoints, cfg *config.Config) ResolvedEndpoints
```

#### API — `internal/api/parameters.go`

Admin-only (401/403) for mutating endpoints; any authenticated user for `GET /instance`.

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET`  | `/api/v1/parameters` | admin | List all active params; optional `?group_key=` filter |
| `GET`  | `/api/v1/parameters/:group/:key` | admin | Get single param value |
| `PUT`  | `/api/v1/parameters/:group/:key` | admin | Upsert `{"value": "..."}` |
| `DELETE` | `/api/v1/parameters/:group/:key` | admin | Soft delete |
| `GET`  | `/api/v1/instance` | any auth | Returns instance info (see below) |
| `PUT`  | `/api/v1/instance/public` | admin | Atomic upsert of all `public.*` params from `PublicEndpoints` body |

`GET /api/v1/instance` response shape:
```json
{
  "listen": {
    "pg":    ":5434",
    "ora":   ":1522",
    "mysql": ":3307",
    "api":   ":4200"
  },
  "public": {
    "host":       "dbbat.example.com",
    "pg_host":    "",
    "ora_host":   "",
    "mysql_host": "",
    "pg_port":    null,
    "ora_port":   null,
    "mysql_port": null
  },
  "resolved": {
    "pg_host":    "dbbat.example.com",
    "pg_port":    5434,
    "ora_host":   "dbbat.example.com",
    "ora_port":   1522,
    "mysql_host": "dbbat.example.com",
    "mysql_port": 3307
  }
}
```
Non-admin callers receive only `listen` and `resolved` (not `public`). The `listen` object is always sourced from `cfg`, never from the DB.

Wire the new routes in `internal/api/server.go` alongside the existing resource groups (`~:204-246`).

Update `internal/api/openapi.yml` with:
- `GlobalParameter` schema
- `PublicEndpoints` schema
- `ResolvedEndpoints` schema
- `InstanceInfo` schema
- Paths for all 6 endpoints above

### Frontend

#### Admin settings page — `front/src/routes/_authenticated/settings/index.tsx`

New TanStack Router route (file-based routing picks it up automatically).

**Sidebar**: add a "Settings" link under the existing "Settings" sidebar group in `front/src/components/AppSidebar.tsx`. The group currently holds only "API Keys"; after this change it holds `[Settings, API Keys]`.

**Access**: admin-only. Non-admins see a placeholder card ("Settings are only available to administrators.") — same gate pattern used elsewhere in the app.

**Page layout** (three sections):

1. **Local listeners** (read-only)
   - Table: Protocol | Bound address
   - Rows: PostgreSQL `:5434`, Oracle `:1522`, MySQL `:3307`, API `:4200` from `instance.listen`.
   - Grey background, helper text: "These are the addresses the server is bound to. Change them via `DBB_LISTEN_*` environment variables and restart."
   - `data-testid="local-listeners-section"`

2. **Public advertisement** (editable form)
   - `public_host` text input: label "Default public host", placeholder "e.g. dbbat.example.com". `data-testid="public-host-input"`.
   - Three expandable protocol rows (PG / Oracle / MySQL), each collapsible via a `<Switch>` "Override for this protocol":
     - Host override input (empty = use default host). `data-testid="public-pg-host-input"` etc.
     - Port override input (integer, empty = use local port). `data-testid="public-pg-port-input"` etc.
     - Inline `<Badge variant="secondary">` showing the resolved value live (recomputed client-side using the same fallback logic as the backend).
   - Save button (`data-testid="save-public-settings-btn"`). Calls `useUpdateInstancePublic()`. On success: `toast.success("Settings saved")`.

3. **Raw parameters** (collapsed by default via `<Collapsible>`)
   - Table: Group | Key | Value (truncated) | Actions
   - Actions per row: Edit (opens a small `<Dialog>` with a `<Textarea>` for the value) + Delete (with `AlertDialog` confirmation).
   - No "Add" button — parameters should be added via the typed sections above or future feature specs.
   - `data-testid="raw-parameters-section"`

**Hooks** in `front/src/api/queries.ts`:
- `useInstance()` → `GET /instance`
- `useUpdateInstancePublic(options?)` → `PUT /instance/public`
- `useParameters(groupKey?)` → `GET /parameters?group_key={groupKey}`
- `useUpdateParameter(options?)` → `PUT /parameters/:group/:key`
- `useDeleteParameter(options?)` → `DELETE /parameters/:group/:key`

All `data-testid` attributes use kebab-case.

## Testing

### Unit tests

**`internal/store/global_parameters_test.go`**:
- Set and get a parameter
- Update existing parameter (upsert)
- Get all parameters for a group
- Soft delete: parameter not found after delete; new parameter with same key can be created
- Unique constraint: two simultaneous live rows with the same (group_key, key) are rejected
- `enc:` prefix round-trip: value stored verbatim with prefix, returned verbatim (no decryption in basic methods)

**`internal/store/global_parameters_test.go`** (typed wrapper):
- `GetPublicEndpoints` returns empty struct when no rows exist
- `SetPublicEndpoints` + `GetPublicEndpoints` round-trip
- `ResolvePublicEndpoints`: protocol override → falls back to default host; port override → falls back to local listen port; empty listen → port 0

### E2E

- Log in as `admin`/`admintest`, navigate to `/settings`.
- Set `public_host=foo.local`, save, reload page, assert input still shows `foo.local`.
- Log in as `viewer`/`viewer`, navigate to `/settings`, assert admin-only placeholder is shown.

### Build

`make test && make lint && make build-app`

## Implementation Plan

1. **Migration** — Create `20260514000000_global_parameters.up.sql` and `.down.sql` with the table, indexes, and `--bun:split` directives. Add `global_parameters` to `Store.DropAllTables`.
2. **Go model** — Add `GlobalParameter` struct to `internal/store/models.go`.
3. **Store CRUD** — Create `internal/store/global_parameters.go` with `GetParameter`, `GetParameters`, `SetParameter`, `DeleteParameter`, plus `PublicEndpoints`/`ResolvedEndpoints` typed wrappers and `ResolvePublicEndpoints`.
4. **Store tests** — Create `internal/store/global_parameters_test.go` covering all unit-test cases from the spec.
5. **API handlers** — Create `internal/api/parameters.go` with 6 endpoints: list/get/put/delete parameters, GET instance, PUT instance/public.
6. **Wire routes** — Register the new routes in `internal/api/server.go`.
7. **OpenAPI** — Add `GlobalParameter`, `PublicEndpoints`, `ResolvedEndpoints`, `InstanceInfo` schemas and all 6 paths to `internal/api/openapi.yml`.
8. **Frontend hooks** — Add `useInstance`, `useUpdateInstancePublic`, `useParameters`, `useUpdateParameter`, `useDeleteParameter` to `front/src/api/queries.ts`.
9. **Frontend settings page** — Create `front/src/routes/_authenticated/settings/index.tsx` with the three sections (local listeners, public advertisement, raw parameters).
10. **Sidebar** — Add Settings link before API Keys in `front/src/components/AppSidebar.tsx`.
11. **QA** — Run `make test lint build-app` and fix any issues.
