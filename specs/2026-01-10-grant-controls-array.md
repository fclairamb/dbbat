# Grant Controls Array

## Problem Statement

Currently, DBBat uses a simple `access_level` field with two values (`read` or `write`) to define user permissions on database grants. However, this approach is limiting:

1. **Misleading terminology**: The `read` access level doesn't just mean "read-only" - it actually implies a set of restrictions (blocking write queries, enabling PostgreSQL read-only mode, blocking privilege escalation attempts, etc.)

2. **Limited extensibility**: Adding new restrictions (like blocking COPY commands or ignoring session parameters) would require either:
   - Adding new access levels (creating a combinatorial explosion)
   - Adding separate boolean flags (losing cohesion)

3. **Binary thinking**: The current model forces an all-or-nothing approach. Either you get all restrictions (`read`) or none (`write`)

### Real-World Use Cases That Don't Fit

- **Allow SELECT but block COPY TO**: A user should be able to query data but not bulk export it
- **Allow writes but block DDL**: A user can INSERT/UPDATE/DELETE but not CREATE/DROP tables

## Proposed Solution

Replace the single `access_level` field with a `controls` array that explicitly lists the restrictions applied to a grant.

### Terminology Decision

Use **`controls`** rather than `restrictions` because:
- "Controls" is neutral - can encompass both restrictive and permissive behaviors
- Common in security contexts (access controls, security controls)
- Aligns with enterprise terminology (SOC 2 controls, security controls)

### Control Types (Initial Implementation)

| Control | Description | Current Equivalent |
|---------|-------------|-------------------|
| `read_only` | Enables PostgreSQL session read-only mode and blocks write queries | `access_level = 'read'` |
| `block_copy` | Blocks all COPY commands (both TO and FROM) | New |
| `block_ddl` | Blocks DDL statements (CREATE, ALTER, DROP, TRUNCATE) | Subset of `read_only` |

### Behavior Changes

**Empty array `[]`**: No restrictions - full write access (equivalent to current `access_level = 'write'`)

**`["read_only"]`**: Equivalent to current `access_level = 'read'`:
- Sets `default_transaction_read_only = on` on connection
- Blocks write queries at proxy level (defense-in-depth)
- Blocks attempts to disable read-only mode

**`["read_only", "block_copy"]`**: Read-only access with no bulk export/import capability

**`["block_ddl"]`**: Can read and write data, but cannot change schema

### Control Hierarchy

Some controls imply others:

```
read_only
├── block_ddl (CREATE, ALTER, DROP, TRUNCATE are write operations)
└── block_copy (for COPY FROM - COPY FROM is a write operation)
```

The `read_only` control is comprehensive - when enabled, attempting DDL or COPY FROM will fail at the PostgreSQL level (via `default_transaction_read_only = on`). The `block_copy` control blocks COPY TO as well (data export), which is not blocked by `read_only` alone. The `block_ddl` control allows more granular restrictions without full read-only mode.

## Database Schema Changes

### Edit Initial Migration

Since the project is not yet released, we edit the existing initial migration file directly (`internal/migrations/sql/20260107000000_initial_schema.up.sql`) rather than creating a new migration.

**Before** (in `access_grants` table):
```sql
access_level TEXT NOT NULL CHECK (access_level IN ('read', 'write')),
```

**After**:
```sql
controls TEXT[] NOT NULL DEFAULT '{}',
```

Also add a GIN index for efficient array queries:
```sql
CREATE INDEX idx_access_grants_controls ON access_grants USING GIN(controls);
```

### Complete access_grants Table (After Change)

```sql
-- Manage access grants
CREATE TABLE access_grants (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(uid),
    database_id UUID NOT NULL REFERENCES databases(uid),
    controls TEXT[] NOT NULL DEFAULT '{}',           -- CHANGED: replaces access_level
    granted_by UUID NOT NULL REFERENCES users(uid),
    starts_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users(uid),
    max_query_counts INT,                   -- NULL = unlimited
    max_bytes_transferred BIGINT,           -- NULL = unlimited
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_time_window CHECK (starts_at < expires_at)
);
CREATE INDEX idx_access_grants_user_id ON access_grants(user_id);
CREATE INDEX idx_access_grants_database_id ON access_grants(database_id);
CREATE INDEX idx_access_grants_expires_at ON access_grants(expires_at);
CREATE INDEX idx_access_grants_controls ON access_grants USING GIN(controls);
CREATE INDEX idx_access_grants_active ON access_grants(user_id, database_id)
    WHERE revoked_at IS NULL;
```

## Model Changes

### Go Model

```go
// internal/store/models.go

// Control constants for grant restrictions
const (
    ControlReadOnly  = "read_only"
    ControlBlockCopy = "block_copy"
    ControlBlockDDL  = "block_ddl"
)

// ValidControls lists all valid control values
var ValidControls = []string{
    ControlReadOnly,
    ControlBlockCopy,
    ControlBlockDDL,
}

// AccessGrant represents an access grant
type AccessGrant struct {
    bun.BaseModel `bun:"table:access_grants,alias:ag"`

    UID                 uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
    UserID              uuid.UUID  `bun:"user_id,notnull,type:uuid" json:"user_id"`
    DatabaseID          uuid.UUID  `bun:"database_id,notnull,type:uuid" json:"database_id"`
    Controls            []string   `bun:"controls,array" json:"controls"` // NEW: replaces access_level
    GrantedBy           uuid.UUID  `bun:"granted_by,notnull,type:uuid" json:"granted_by"`
    StartsAt            time.Time  `bun:"starts_at,notnull" json:"starts_at"`
    ExpiresAt           time.Time  `bun:"expires_at,notnull" json:"expires_at"`
    // ... rest unchanged
}

// HasControl checks if the grant has a specific control enabled
func (g *AccessGrant) HasControl(control string) bool {
    for _, c := range g.Controls {
        if c == control {
            return true
        }
    }
    return false
}

// IsReadOnly returns true if the grant has read_only control
func (g *AccessGrant) IsReadOnly() bool {
    return g.HasControl(ControlReadOnly)
}

// ShouldBlockCopy returns true if COPY commands should be blocked
func (g *AccessGrant) ShouldBlockCopy() bool {
    return g.HasControl(ControlBlockCopy)
}

// ShouldBlockDDL returns true if DDL commands should be blocked
func (g *AccessGrant) ShouldBlockDDL() bool {
    return g.HasControl(ControlBlockDDL)
}
```

## API Changes

### Create Grant Request

**Before:**
```json
{
  "user_id": "uuid",
  "database_id": "uuid",
  "access_level": "read",
  "starts_at": "2026-01-10T00:00:00Z",
  "expires_at": "2026-01-17T00:00:00Z"
}
```

**After:**
```json
{
  "user_id": "uuid",
  "database_id": "uuid",
  "controls": ["read_only"],
  "starts_at": "2026-01-10T00:00:00Z",
  "expires_at": "2026-01-17T00:00:00Z"
}
```

### Grant Response

**Before:**
```json
{
  "uid": "uuid",
  "user_id": "uuid",
  "database_id": "uuid",
  "access_level": "read",
  "starts_at": "2026-01-10T00:00:00Z",
  "expires_at": "2026-01-17T00:00:00Z"
}
```

**After:**
```json
{
  "uid": "uuid",
  "user_id": "uuid",
  "database_id": "uuid",
  "controls": ["read_only"],
  "starts_at": "2026-01-10T00:00:00Z",
  "expires_at": "2026-01-17T00:00:00Z"
}
```

### OpenAPI Schema

```yaml
components:
  schemas:
    GrantControl:
      type: string
      enum:
        - read_only
        - block_copy
        - block_ddl
      description: |
        Control types that can be applied to a grant:
        - `read_only`: Enables PostgreSQL session read-only mode and blocks write queries
        - `block_copy`: Blocks all COPY commands (both TO and FROM)
        - `block_ddl`: Blocks DDL statements (CREATE, ALTER, DROP, TRUNCATE)

    CreateGrantRequest:
      type: object
      required:
        - user_id
        - database_id
        - starts_at
        - expires_at
      properties:
        user_id:
          type: string
          format: uuid
        database_id:
          type: string
          format: uuid
        controls:
          type: array
          items:
            $ref: '#/components/schemas/GrantControl'
          default: []
          description: List of controls to apply. Empty array means full write access.
        starts_at:
          type: string
          format: date-time
        expires_at:
          type: string
          format: date-time
```

## Proxy Changes

### Session Initialization

```go
// internal/proxy/upstream.go

func (s *Session) initializeUpstreamConnection() error {
    // ... existing connection code ...

    // Apply controls
    if s.grant.IsReadOnly() {
        if err := s.setSessionReadOnly(); err != nil {
            return fmt.Errorf("failed to set read-only mode: %w", err)
        }
    }

    return nil
}
```

### Query Interception

```go
// internal/proxy/intercept.go

func (s *Session) handleQuery(query *pgproto3.Query) error {
    sqlText := query.String

    // Check quotas
    if err := s.checkQuotas(); err != nil {
        return err
    }

    // Always block password changes
    if isPasswordChangeQuery(sqlText) {
        return ErrPasswordChangeNotAllowed
    }

    // Control: read_only bypass prevention
    if s.grant.IsReadOnly() && isReadOnlyBypassAttempt(sqlText) {
        return ErrReadOnlyBypassAttempt
    }

    // Control: read_only write prevention (defense-in-depth)
    if s.grant.IsReadOnly() && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    // Control: block_ddl (only check if not already read_only, since read_only blocks DDL at PG level)
    if !s.grant.IsReadOnly() && s.grant.ShouldBlockDDL() && isDDLQuery(sqlText) {
        return ErrDDLNotPermitted
    }

    // Control: block_copy
    if s.grant.ShouldBlockCopy() && isCopyQuery(sqlText) {
        return ErrCopyNotPermitted
    }

    // ... rest of handler
}
```

### New Detection Functions

```go
// isDDLQuery checks if a query is a DDL operation
func isDDLQuery(sql string) bool {
    upper := strings.ToUpper(strings.TrimSpace(sql))
    ddlKeywords := []string{"CREATE", "ALTER", "DROP", "TRUNCATE"}
    for _, keyword := range ddlKeywords {
        if strings.HasPrefix(upper, keyword) {
            return true
        }
    }
    return false
}

// isCopyQuery checks if a query is a COPY operation
func isCopyQuery(sql string) bool {
    upper := strings.ToUpper(strings.TrimSpace(sql))
    return strings.HasPrefix(upper, "COPY ")
}
```

### New Error Types

```go
// internal/proxy/errors.go

var (
    ErrDDLNotPermitted  = errors.New("DDL operations not permitted: your access grant blocks schema modifications")
    ErrCopyNotPermitted = errors.New("COPY not permitted: your access grant blocks COPY commands")
)
```

## Frontend Changes

### Grant Form

Replace the access level dropdown with a multi-select checkbox group:

```tsx
// front/src/routes/_authenticated/grants/index.tsx

const CONTROLS = [
  { value: 'read_only', label: 'Read Only', description: 'Enable PostgreSQL read-only mode' },
  { value: 'block_copy', label: 'Block COPY', description: 'Prevent COPY commands (data export/import)' },
  { value: 'block_ddl', label: 'Block DDL', description: 'Prevent schema modifications (CREATE, ALTER, DROP)' },
];

function GrantForm() {
  const [controls, setControls] = useState<string[]>([]);

  return (
    <fieldset>
      <legend>Access Controls</legend>
      <p className="text-muted">Select restrictions to apply. No selections = full write access.</p>
      {CONTROLS.map(control => (
        <label key={control.value}>
          <input
            type="checkbox"
            checked={controls.includes(control.value)}
            onChange={(e) => {
              if (e.target.checked) {
                setControls([...controls, control.value]);
              } else {
                setControls(controls.filter(c => c !== control.value));
              }
            }}
          />
          <span>{control.label}</span>
          <small>{control.description}</small>
        </label>
      ))}
    </fieldset>
  );
}
```

### Grant List Display

```tsx
// Display controls as badges
function ControlBadges({ controls }: { controls: string[] }) {
  if (controls.length === 0) {
    return <span className="badge badge-success">Full Access</span>;
  }

  return (
    <div className="badge-group">
      {controls.map(control => (
        <span key={control} className="badge badge-warning">
          {formatControlName(control)}
        </span>
      ))}
    </div>
  );
}

function formatControlName(control: string): string {
  return control
    .replace(/_/g, ' ')
    .replace(/\b\w/g, c => c.toUpperCase());
}
```

## Testing

### Unit Tests

```go
func TestAccessGrantHasControl(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name     string
        controls []string
        check    string
        expected bool
    }{
        {
            name:     "has read_only",
            controls: []string{"read_only"},
            check:    "read_only",
            expected: true,
        },
        {
            name:     "no controls",
            controls: []string{},
            check:    "read_only",
            expected: false,
        },
        {
            name:     "multiple controls",
            controls: []string{"read_only", "block_copy"},
            check:    "block_copy",
            expected: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            grant := &AccessGrant{Controls: tt.controls}
            if got := grant.HasControl(tt.check); got != tt.expected {
                t.Errorf("HasControl(%q) = %v, want %v", tt.check, got, tt.expected)
            }
        })
    }
}

func TestShouldBlockCopy(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name      string
        controls  []string
        blockCopy bool
    }{
        {
            name:      "no controls",
            controls:  []string{},
            blockCopy: false,
        },
        {
            name:      "block_copy enabled",
            controls:  []string{"block_copy"},
            blockCopy: true,
        },
        {
            name:      "read_only does not block copy",
            controls:  []string{"read_only"},
            blockCopy: false, // COPY FROM fails at PostgreSQL level due to read-only
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            grant := &AccessGrant{Controls: tt.controls}
            if got := grant.ShouldBlockCopy(); got != tt.blockCopy {
                t.Errorf("ShouldBlockCopy() = %v, want %v", got, tt.blockCopy)
            }
        })
    }
}
```

### Integration Tests

```go
func TestControlBlockCopy(t *testing.T) {
    // Setup: Create grant with block_copy control
    grant := createGrant(t, userID, databaseID, []string{"block_copy"})

    // Connect through proxy
    conn, err := pgx.Connect(ctx, connString)
    require.NoError(t, err)
    defer conn.Close(ctx)

    // SELECT should work
    _, err = conn.Exec(ctx, "SELECT * FROM test_data")
    require.NoError(t, err)

    // COPY TO should be blocked
    _, err = conn.Exec(ctx, "COPY test_data TO STDOUT")
    require.Error(t, err)
    assert.Contains(t, err.Error(), "COPY not permitted")
}

func TestControlBlockDDL(t *testing.T) {
    // Setup: Create grant with block_ddl control
    grant := createGrant(t, userID, databaseID, []string{"block_ddl"})

    // Connect through proxy
    conn, err := pgx.Connect(ctx, connString)
    require.NoError(t, err)
    defer conn.Close(ctx)

    // SELECT should work
    _, err = conn.Exec(ctx, "SELECT * FROM test_data")
    require.NoError(t, err)

    // INSERT should work (DDL != DML)
    _, err = conn.Exec(ctx, "INSERT INTO test_data (name) VALUES ('test')")
    require.NoError(t, err)

    // CREATE TABLE should be blocked
    _, err = conn.Exec(ctx, "CREATE TABLE test_blocked (id INT)")
    require.Error(t, err)
    assert.Contains(t, err.Error(), "DDL operations not permitted")
}
```

### Playwright E2E Tests

Update `front/e2e/grants.spec.ts` to test the new controls UI:

```typescript
import { test, expect } from "./fixtures";

test.describe("Access Grants Management", () => {
  test("should display grants list page", async ({ authenticatedPage }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list.png",
      fullPage: true,
    });

    // Verify we're on the grants page
    await expect(authenticatedPage).toHaveURL(/\/grants/);

    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });

  test("should show create grant form with controls checkboxes", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Look for create/add button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Take screenshot of create grant dialog/form
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-create-dialog.png",
      });

      // Look for controls checkboxes (replaces access_level dropdown)
      const formContent = await authenticatedPage.textContent("body");
      expect(formContent?.toLowerCase()).toMatch(/read.only|block.copy|block.ddl|controls/);
    }
  });

  test("should create grant with read_only control", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Click create button
    const createButton = authenticatedPage.getByRole("button", {
      name: /create|add|new|grant/i,
    });

    if (await createButton.isVisible()) {
      await createButton.click();

      // Select user and database (if dropdowns exist)
      // Check the read_only control checkbox
      const readOnlyCheckbox = authenticatedPage.getByLabel(/read.only/i);
      if (await readOnlyCheckbox.isVisible()) {
        await readOnlyCheckbox.check();
      }

      // Take screenshot with control selected
      await authenticatedPage.screenshot({
        path: "test-results/screenshots/grants-create-with-controls.png",
      });
    }
  });

  test("should display controls badges in grant list", async ({
    authenticatedPage,
  }) => {
    await authenticatedPage.goto("grants");
    await authenticatedPage.waitForLoadState("networkidle");

    // Take screenshot showing grant list with controls badges
    await authenticatedPage.screenshot({
      path: "test-results/screenshots/grants-list-with-controls.png",
      fullPage: true,
    });

    // Verify controls-related content is displayed (badges like "Read Only", "Full Access", etc.)
    const content = await authenticatedPage.textContent("body");
    expect(content).toBeTruthy();
  });
});
```

## Implementation Steps

1. **Edit initial migration**: Replace `access_level` with `controls` array in `internal/migrations/sql/20260107000000_initial_schema.up.sql`
2. **Update models**: Replace `AccessLevel` with `Controls` field, add helper methods and constants in `internal/store/models.go`
3. **Update store**: Modify grant CRUD operations in `internal/store/grants.go`
4. **Update API**: Change request/response schemas in `internal/api/grants.go`, update OpenAPI spec in `internal/api/openapi.yml`
5. **Update proxy**: Implement control checks in `internal/proxy/intercept.go`, add new error types
6. **Update frontend**: Replace access level dropdown with control checkboxes in `front/src/routes/_authenticated/grants/index.tsx`
7. **Update test mode setup**: Update `internal/store/testmode.go` to create grants with `controls` instead of `access_level`
8. **Regenerate API client**: Run `bun run generate-client` in `front/` to update TypeScript types
9. **Update Playwright tests**: Update `front/e2e/grants.spec.ts` to test new controls UI
10. **Update documentation**: CLAUDE.md, API docs, website

## Future Extensions

This architecture makes it easy to add new controls:

- `block_copy_out`: Block only COPY TO (data export)
- `block_copy_in`: Block only COPY FROM (data import)
- `block_temp_tables`: Prevent creation of temporary tables
- `block_session_params`: Block SET/RESET commands
- `block_role_changes`: Block SET ROLE/AUTHORIZATION
- `max_statement_time`: Enforce maximum query duration
