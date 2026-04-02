# Oracle UI Protocol Support

## Goal

Update the database management UI to support creating and displaying Oracle database configurations. The form should adapt its fields based on the selected protocol, and the database list should show the protocol type.

## Prerequisites

- API spec `oracle-api-protocol-support.md` must be implemented first
- TypeScript types regenerated from updated OpenAPI spec (`bun run generate-client`)

## Outcome

- Protocol selector dropdown at the top of the Create Database form
- Form fields adapt based on protocol: PostgreSQL shows SSL Mode, Oracle shows Service Name
- Default port changes when protocol is selected (5432 → 1521)
- Database list table shows protocol column with visual indicator
- Existing PostgreSQL workflow unchanged

---

## Changes

### File: `front/src/routes/_authenticated/databases/index.tsx`

### 1. CreateDatabaseDialog — Add Protocol State

Add new state variables after existing ones (~line 181):

```tsx
const [protocol, setProtocol] = useState("postgresql");
const [oracleServiceName, setOracleServiceName] = useState("");
```

### 2. CreateDatabaseDialog — Protocol Selector

Add a protocol selector as the **first field** in the form (before Name), so all subsequent fields adapt:

```tsx
<div className="space-y-2">
  <Label htmlFor="protocol">Protocol</Label>
  <Select
    value={protocol}
    onValueChange={(val) => {
      setProtocol(val);
      // Update default port when protocol changes
      if (val === "oracle" && port === "5432") {
        setPort("1521");
      } else if (val === "postgresql" && port === "1521") {
        setPort("5432");
      }
    }}
  >
    <SelectTrigger>
      <SelectValue />
    </SelectTrigger>
    <SelectContent>
      <SelectItem value="postgresql">PostgreSQL</SelectItem>
      <SelectItem value="oracle">Oracle</SelectItem>
    </SelectContent>
  </Select>
</div>
```

### 3. CreateDatabaseDialog — Conditional Fields

#### Database Name field (~line 258)
Show for PostgreSQL, hide for Oracle:

```tsx
{protocol === "postgresql" && (
  <div className="space-y-2">
    <Label htmlFor="databaseName">Database Name</Label>
    <Input
      id="databaseName"
      value={databaseName}
      onChange={(e) => setDatabaseName(e.target.value)}
      placeholder="myapp"
      required
    />
  </div>
)}
```

#### Oracle Service Name field (new)
Show only for Oracle, after the Host/Port row:

```tsx
{protocol === "oracle" && (
  <div className="space-y-2">
    <Label htmlFor="oracleServiceName">Service Name</Label>
    <Input
      id="oracleServiceName"
      value={oracleServiceName}
      onChange={(e) => setOracleServiceName(e.target.value)}
      placeholder="ORCL"
      required
    />
  </div>
)}
```

#### SSL Mode field (~line 288)
Show only for PostgreSQL:

```tsx
{protocol === "postgresql" && (
  <div className="space-y-2">
    <Label htmlFor="sslMode">SSL Mode</Label>
    <Select value={sslMode} onValueChange={setSslMode}>
      {/* ... existing options ... */}
    </Select>
  </div>
)}
```

#### Username placeholder
Change dynamically:

```tsx
placeholder={protocol === "oracle" ? "SYSTEM" : "postgres"}
```

### 4. CreateDatabaseDialog — Update Submit Payload

Update `handleSubmit` (~line 193):

```tsx
const handleSubmit = (e: React.FormEvent) => {
  e.preventDefault();
  createDb.mutate({
    name,
    description: description || undefined,
    host,
    port: parseInt(port, 10),
    database_name: protocol === "postgresql" ? databaseName : oracleServiceName,
    username,
    password,
    ssl_mode: protocol === "postgresql" ? sslMode : undefined,
    protocol,
    oracle_service_name: protocol === "oracle" ? oracleServiceName : undefined,
  });
};
```

### 5. DataTable — Add Protocol Column

Add a new column before the Host column in the `columns` array (~line 71):

```tsx
{
  key: "protocol",
  header: "Type",
  cell: (db) =>
    isFullDatabase(db) ? (
      <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${
        db.protocol === "oracle"
          ? "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"
          : "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"
      }`}>
        {db.protocol === "oracle" ? "Oracle" : "PostgreSQL"}
      </span>
    ) : (
      <span className="text-muted-foreground">-</span>
    ),
},
```

### 6. DataTable — Update Database Column

The "Database" column should show `oracle_service_name` for Oracle databases:

```tsx
{
  key: "database_name",
  header: "Database",
  cell: (db) =>
    isFullDatabase(db) ? (
      <span className="font-mono text-sm">
        {db.protocol === "oracle"
          ? db.oracle_service_name || db.database_name
          : db.database_name}
      </span>
    ) : (
      <span className="text-muted-foreground">-</span>
    ),
},
```

### 7. DataTable — Hide SSL Column for Oracle

Update the SSL column to show "-" for Oracle:

```tsx
{
  key: "ssl_mode",
  header: "SSL",
  cell: (db) =>
    isFullDatabase(db) && db.protocol !== "oracle" ? (
      <span className="text-sm">{db.ssl_mode}</span>
    ) : (
      <span className="text-muted-foreground">-</span>
    ),
},
```

### 8. Type Guard Update

Update `isFullDatabase` to include protocol:

```tsx
function isFullDatabase(db: DatabaseItem): db is Database {
  return "host" in db;
}
```

No change needed — `protocol` will be part of `Database` type after regeneration.

---

## Type Regeneration

After the API spec changes are implemented:

```bash
cd front && bun run generate-client
```

This updates `front/src/api/schema.ts` with the new `protocol` and `oracle_service_name` fields in:
- `Database` response type
- `CreateDatabaseRequest` type
- `UpdateDatabaseRequest` type

---

## Visual Layout

### PostgreSQL (default):
```
┌──────────────────────────────────┐
│ Protocol:    [PostgreSQL ▾]      │
│ Name:        [____________]      │
│ Description: [____________]      │
│ Host: [__________] Port: [5432]  │
│ Database Name: [__________]      │
│ Username:    [____________]      │
│ Password:    [____________]      │
│ SSL Mode:    [Prefer ▾]         │
│                    [Cancel] [Create] │
└──────────────────────────────────┘
```

### Oracle:
```
┌──────────────────────────────────┐
│ Protocol:    [Oracle ▾]          │
│ Name:        [____________]      │
│ Description: [____________]      │
│ Host: [__________] Port: [1521]  │
│ Service Name: [__________]       │
│ Username:    [____________]      │
│ Password:    [____________]      │
│                    [Cancel] [Create] │
└──────────────────────────────────┘
```

---

## Tests

### E2E tests to add/update in `front/e2e/databases.spec.ts`:

1. `test("should create PostgreSQL database")` — existing test still passes
2. `test("should create Oracle database")` — select Oracle, fill service name, verify creation
3. `test("should show protocol column in database list")` — verify badge renders
4. `test("should adapt form fields when switching protocol")` — select Oracle → SSL hidden, Service Name shown; switch back → SSL shown, Service Name hidden
5. `test("should update default port on protocol change")` — select Oracle → port changes to 1521

---

## Files

| File | Change |
|------|--------|
| `front/src/routes/_authenticated/databases/index.tsx` | Protocol selector, conditional fields, table columns |
| `front/src/api/schema.ts` | Auto-generated from OpenAPI spec |

## Acceptance Criteria

1. Protocol dropdown with PostgreSQL (default) and Oracle options
2. Selecting Oracle hides SSL Mode, shows Service Name, changes port to 1521
3. Selecting PostgreSQL shows SSL Mode, hides Service Name, changes port to 5432
4. Database list shows "Type" column with colored badges (blue=PostgreSQL, red=Oracle)
5. Database column shows `oracle_service_name` for Oracle databases
6. Form submission sends correct protocol-specific fields
7. Existing PostgreSQL workflow is unchanged
8. `bun run build` compiles without TypeScript errors
