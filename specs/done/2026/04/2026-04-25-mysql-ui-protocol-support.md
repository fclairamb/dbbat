# MySQL UI Protocol Support

> Parent spec: `2026-04-25-mysql-proxy.md`
> Can be implemented in parallel with Phase 1 (after the API spec lands).

## Goal

Surface MySQL as a selectable protocol in the database create/edit forms and show it correctly in list/detail views. Make port input protocol-aware (suggest 5432/1521/3306).

## Files modified

```
front/src/api/schema.ts                                    # regenerated from openapi.yml
front/src/routes/_authenticated/databases/index.tsx        # list view (protocol icon/text)
front/src/routes/_authenticated/databases/$databaseId.tsx  # detail view
front/src/components/databases/DatabaseForm.tsx            # create/edit form (or wherever the form lives)
front/src/components/databases/protocolDefaults.ts         # NEW — port defaults by protocol
```

(Exact filenames may differ; update during implementation. The substance is: protocol dropdown, port default, list rendering.)

## Schema regeneration

After the API spec lands and `openapi.yml` includes `mysql`, run the schema generator:

```bash
cd front && bun run gen:schema
```

This updates `front/src/api/schema.ts` so the TypeScript `protocol` type is `"postgresql" | "oracle" | "mysql"`.

## Form changes

### Protocol dropdown

```tsx
<select name="protocol" value={protocol} onChange={...}>
  <option value="postgresql">PostgreSQL</option>
  <option value="oracle">Oracle</option>
  <option value="mysql">MySQL</option>
</select>
```

### Protocol-aware port default

```ts
// front/src/components/databases/protocolDefaults.ts
export const PROTOCOL_DEFAULT_PORT: Record<Protocol, number> = {
  postgresql: 5432,
  oracle: 1521,
  mysql: 3306,
};

export const PROTOCOL_LABEL: Record<Protocol, string> = {
  postgresql: "PostgreSQL",
  oracle: "Oracle",
  mysql: "MySQL",
};
```

In the form, when the user changes the protocol dropdown, set the port input value to `PROTOCOL_DEFAULT_PORT[newProtocol]` **only if** the user hasn't explicitly edited the port (track with a flag).

### Hide Oracle-specific fields when not Oracle

The form currently shows `oracle_service_name` only when `protocol === "oracle"`. No change needed for MySQL — MySQL has no protocol-specific fields beyond what's shared.

## List view

`databases/index.tsx` currently has logic like:

```tsx
{db.protocol === "oracle"
  ? db.oracle_service_name || db.database_name
  : db.database_name}
```

Extend the protocol display:

```tsx
<ProtocolBadge protocol={db.protocol} />
```

Where `ProtocolBadge` renders an icon (or label) per protocol. If a badge component doesn't exist yet, use simple text:

```tsx
<span>{PROTOCOL_LABEL[db.protocol] ?? db.protocol}</span>
```

## Detail view

The Database Detail page shows protocol-specific fields. For MySQL, show `database_name`, `host`, `port`, `username`, `ssl_mode`. No MySQL-specific fields.

## Tests (Playwright E2E)

Add to existing `tests/databases.spec.ts` or similar:

- `test('admin can create a MySQL database', ...)` — fills the form with protocol=mysql, port auto-fills to 3306, submits, sees the new database in the list
- `test('protocol switch updates port default', ...)` — switches dropdown PG→MySQL→Oracle, verifies port input changes 5432→3306→1521 (when not user-edited)

## Verification checklist

- [ ] `cd front && bun run typecheck` clean
- [ ] `cd front && bun run lint` clean
- [ ] `make test-e2e` passes
- [ ] Manual: in dev mode, create a MySQL database, verify it appears in the list with correct protocol label
- [ ] Manual: edit the MySQL database, verify form pre-fills correctly

## Out of scope

- Custom protocol icons (use text label until requested)
- Per-protocol "test connection" button (separate spec)
