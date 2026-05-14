# Connection URLs

## Status: Todo

## Dependencies

Requires `2026-05-14-global-parameters.md` to be implemented first (needs `store.ResolvedEndpoints` and `store.GetPublicEndpoints`).

## Summary

Everywhere dbbat hands a user an API key or shows them a database, display a ready-to-paste connection URL. In the "API Key Created" dialog, embed the cleartext key into a full URL for every database the key's owner has an active grant on. In the database detail dialog, show a template URL with `{API_KEY}` placeholder.

## Solution

### Backend

#### URL builder — `internal/api/connection_url.go`

```go
type ConnectionInfo struct {
    DatabaseUID  uuid.UUID `json:"database_uid"`
    DatabaseName string    `json:"database_name"`
    Protocol     string    `json:"protocol"`
    Format       string    `json:"format"` // "uri" | "ez-connect"
    URL          string    `json:"url"`
}

// BuildConnectionURL builds a connection URL for the given database, user, and key.
// apiKey == "" substitutes the placeholder "{API_KEY}".
// Returns (info, false) when the protocol's resolved port is 0 (proxy disabled).
func BuildConnectionURL(
    db *store.Database,
    user *store.User,
    endpoints store.ResolvedEndpoints,
    apiKey string,
) (ConnectionInfo, bool)
```

URL formats per protocol:

| Protocol | Format | URL template |
|----------|--------|-------------|
| `postgresql` | `uri` | `postgresql://{user}:{key}@{host}:{port}/{db_name}` + `?sslmode={mode}` (omit when mode is `prefer`) |
| `mysql` | `uri` | `mysql://{user}:{key}@{host}:{port}/{db_name}` |
| `mariadb` | `uri` | `mysql://{user}:{key}@{host}:{port}/{db_name}` (same listener, same scheme) |
| `oracle` | `ez-connect` | `{user}/{key}@{host}:{port}/{service_or_db}` — use `db.OracleServiceName` if set, else `db.DatabaseName` |

`user` in the URL is `user.Username` (the dbbat account credential, **not** `db.Username` which is the target DB credential dbbat uses internally).

URL-encode the key value in the password slot when building the URI (API keys are alphanumeric so no encoding is strictly needed, but apply it for correctness).

Returns `(ConnectionInfo{}, false)` when the relevant `endpoints.PGPort`/`OraPort`/`MySQLPort` is 0.

#### API key creation — `internal/api/keys.go`

Extend `CreateAPIKeyResponse`:

```go
type CreateAPIKeyResponse struct {
    // existing fields unchanged ...
    Connections        []ConnectionInfo `json:"connections"`
    ConnectionsTruncated bool           `json:"connections_truncated"`
}
```

In `handleCreateAPIKey`, after the key is written and the cleartext key is in hand:
1. Load resolved endpoints: `store.GetPublicEndpoints(ctx)` → `store.ResolvePublicEndpoints(pe, s.config)`.
2. List active grants for the key's user: `store.ListGrants(GrantFilter{UserID: &user.UID, ActiveOnly: true})`.
3. Deduplicate `grant.DatabaseUID`, batch-load `store.GetDatabases(uids...)`.
4. Filter: non-admin users skip databases where `db.Listable == false`.
5. For each database, call `BuildConnectionURL(db, user, endpoints, cleartextKey)`. Collect truthy results.
6. If `len(connections) > 50` set `ConnectionsTruncated = true` and truncate to 50.
7. Attach to the response.

Update `internal/api/openapi.yml`: add `ConnectionInfo` schema; extend `CreateAPIKeyResponse` with `connections`/`connections_truncated` fields.

#### Per-database connection template

New endpoint in `internal/api/databases.go` (or a new `internal/api/database_connection.go`):

```
GET /api/v1/databases/:uid/connection
```

Authorization:
- Admin: always 200.
- Non-admin: 200 if the caller has at least one active grant on this database; **404** (not 403) otherwise — returning 403 leaks that the database exists.

Response: `ConnectionInfo` with `apiKey == ""` (placeholder `{API_KEY}`).

Special cases:
- Protocol proxy disabled (resolved port = 0): return 409 with `{"error": "proxy for this protocol is disabled"}`.
- Database not found: 404.

Update `internal/api/openapi.yml` with the new path and response schema.

Wire both new endpoint groups in `internal/api/server.go`.

### Frontend

#### Shared component — `front/src/components/shared/CopyableField.tsx`

Extract the copy-field pattern currently inlined at `front/src/routes/_authenticated/api-keys/index.tsx:257-309`:

```tsx
interface CopyableFieldProps {
    label?: string
    value: string
    mask?: boolean       // renders ••• visually but copies the real value
    monospace?: boolean  // default true
    toastMessage?: string
    testId?: string
}
```

Behaviour: read-only `<Input>` + `<Button variant="outline" size="icon">` with lucide `Copy` icon. On click: `navigator.clipboard.writeText(value)` then `toast.success(toastMessage ?? "Copied to clipboard")`.

Refactor the existing API-key copy block in `api-keys/index.tsx` to use `<CopyableField>`.

#### API Key Created dialog — `api-keys/index.tsx`

Modify `ShowKeyDialog` to render a "Connection URLs" section below the key field:

```
┌─ API Key Created ─────────────────────────────────────────┐
│ ⚠ This is the only time you will see this key.            │
│                                                            │
│  [dbb_abc...xyz        ] [Copy]                           │
│                                                            │
│  Connection URLs                                           │
│  ┌──────────────────────────────────────────────────────┐ │
│  │ my-pg-db (postgresql)                                │ │
│  │ [postgresql://alice:dbb_abc...@db.example.com:5432/] │ │
│  │                                             [Copy]   │ │
│  ├──────────────────────────────────────────────────────┤ │
│  │ prod-oracle (oracle · EZ-Connect)                    │ │
│  │ [alice/dbb_abc...@ora.example.com:1521/ORCLPDB      ]│ │
│  │                                             [Copy]   │ │
│  └──────────────────────────────────────────────────────┘ │
│                                                            │
│  [Close]                                                   │
└────────────────────────────────────────────────────────────┘
```

- Each row: label `{db.database_name} ({db.protocol}[· EZ-Connect for oracle])` + `<CopyableField value={conn.url} testId="connection-url-{db.database_uid}" />`.
- Empty state (`connections.length === 0`): muted text "No active grants yet — ask an admin to grant you database access before connecting."
- Truncation warning: if `connections_truncated`, show a yellow `<Alert>` "Showing first 50 databases. Use an existing grant to see all connection details."
- `data-testid="connections-section"`.

#### Database detail dialog — `databases/index.tsx`

Add `onRowClick` to `<DataTable>` that opens a `DatabaseDetailsDialog` defined inline in the same file (consistent with `CreateDatabaseDialog` / `DeleteDatabaseDialog`).

Dialog content:

```
┌─ my-pg-db ────────────────────────────────────────────────┐
│ Protocol:    PostgreSQL                                     │
│ Description: Production analytics replica                   │
│                                                            │
│ [admin only section]                                        │
│ Target:      postgres.internal:5432 / analytics_db         │
│ SSL mode:    require                                        │
│                                                            │
│ Connection URL                                             │
│ Use one of your API keys as the password (▸ tooltip)       │
│ [postgresql://alice:{API_KEY}@db.example.com:5432/my-pg-db]│
│                                                     [Copy] │
└────────────────────────────────────────────────────────────┘
```

- `useDatabaseConnection(uid)` hook calls `GET /databases/:uid/connection`.
- When the hook returns 409 (proxy disabled): show an `<Alert variant="warning">` "The proxy for this protocol is currently disabled."
- When 404: hide the connection URL section entirely (user has no grant; don't expose DB existence in the error message).
- Admin always sees the section (BuildConnectionURL is called with `{API_KEY}` placeholder regardless).
- `data-testid="database-details-dialog"`, `data-testid="database-connection-url"`.

**Hooks** in `front/src/api/queries.ts`:
```ts
export function useDatabaseConnection(uid: string | undefined) {
    return useQuery({
        queryKey: ["databases", uid, "connection"],
        queryFn: async (): Promise<ConnectionInfo> => { ... },
        enabled: !!uid,
        retry: false, // 404 is expected when no grant; don't retry
    })
}
```

Also extend the `CreateAPIKeyResponse` type alias so TypeScript picks up `connections: ConnectionInfo[]` (auto-populated after `bun run generate-client`).

## Testing

### Unit tests

**`internal/api/connection_url_test.go`**:
- PostgreSQL URL with `sslmode=require` (include), `sslmode=prefer` (omit), `sslmode=disable` (include).
- MySQL and MariaDB both produce `mysql://` scheme.
- Oracle uses `OracleServiceName` when set; falls back to `DatabaseName`.
- Oracle URL does NOT use `://` URI syntax.
- `apiKey == ""` produces `{API_KEY}` in the password slot.
- Disabled protocol (port 0) returns `(_, false)`.
- Username is the dbbat `user.Username`, not `db.Username`.

**`internal/api/keys_test.go`** (extend existing tests):
- Creating a key for a user with active grants populates `connections`.
- Non-listable database excluded from `connections` for non-admin user.
- Disabled protocol excluded (port 0 after no `public.pg.port` row and empty `cfg.ListenPG`).
- `connections_truncated` set when > 50 grants.

### E2E (`make test-e2e`)

- Log in as `admin`/`admintest`.
- Create an API key via the UI.
- Assert the "API Key Created" dialog contains at least one `<CopyableField>` whose value starts with `postgresql://` and contains the string `dbb_`.
- Click a database row.
- Assert `DatabaseDetailsDialog` opens and contains a URL with `{API_KEY}`.

### Manual

- `make dev`, log in as admin, create a key, copy the PostgreSQL connection URL.
- Paste into `psql` against the local dev PostgreSQL proxy.
- Connection succeeds.

### Build

`make test && make lint && make build-app`
