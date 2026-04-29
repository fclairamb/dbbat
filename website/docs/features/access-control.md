---
sidebar_position: 1
---

# Access Control

DBBat provides fine-grained access control through **grants**. A grant gives a user permission to access a specific database for a limited time, optionally with one or more controls and quotas.

## Creating a Grant

```bash
curl -X POST http://localhost:4200/api/v1/grants \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": "550e8400-e29b-41d4-a716-446655440000",
    "database_id": "660e8400-e29b-41d4-a716-446655440000",
    "controls": ["read_only"],
    "starts_at": "2024-01-15T09:00:00Z",
    "expires_at": "2024-01-15T18:00:00Z",
    "max_query_counts": 1000,
    "max_bytes_transferred": 104857600
  }'
```

## Grant Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `user_id` | UUID | UID of the user | Yes |
| `database_id` | UUID | UID of the database configuration | Yes |
| `controls` | array | Combination of `read_only`, `block_copy`, `block_ddl`. Empty = full write access. | No (default: `[]`) |
| `starts_at` | datetime | When the grant becomes active | Yes |
| `expires_at` | datetime | When the grant expires (must be after `starts_at`) | Yes |
| `max_query_counts` | integer | Maximum number of queries allowed | No |
| `max_bytes_transferred` | integer | Maximum bytes transferred (response size) | No |

The grant model is the same across all engines (PostgreSQL, Oracle, MySQL/MariaDB).

## Controls

Controls are **independent** and **combinable**. A grant with `["read_only", "block_copy", "block_ddl"]` enforces all three. An empty array allows full write access — including DDL, COPY, and writes — within the grant's time window.

### `read_only`

Blocks every operation that mutates data, in **defense-in-depth**:

- **Layer 1 — SQL inspection** (all engines): regex blocks `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `REPLACE`, `CREATE`, `ALTER`, `DROP`, `TRUNCATE`, `GRANT`, `REVOKE`, plus `COPY FROM` (PostgreSQL) and `LOAD DATA` / `SELECT … INTO OUTFILE` (MySQL).
- **Layer 2 — engine session flag**:
  - **PostgreSQL**: `SET SESSION default_transaction_read_only = on` at session start.
  - **MySQL/MariaDB**: regex inspection only — `SET SESSION TRANSACTION READ ONLY` only applies to the *next* transaction in MySQL and is trivially bypassable.
  - **Oracle**: regex inspection only.
- **Layer 3 — bypass prevention** (PostgreSQL): attempts to disable read-only mode are blocked (`SET default_transaction_read_only = off`, `RESET …`, `SET SESSION AUTHORIZATION`, `SET ROLE`).

`read_only` is defense in depth for **trusted users**, not a security boundary against malicious actors. For untrusted access, also limit privileges on the upstream database user (e.g. PostgreSQL `GRANT SELECT` only).

### `block_copy`

Blocks all bulk file-touching operations:

- **PostgreSQL**: `COPY … TO` and `COPY … FROM` (both directions).
- **MySQL/MariaDB**: `LOAD DATA INFILE`, `SELECT … INTO OUTFILE`, `SELECT … INTO DUMPFILE`. Note that `LOAD DATA LOCAL INFILE` is **always** refused (see the MySQL notes).

### `block_ddl`

Blocks schema changes: `CREATE`, `ALTER`, `DROP`, `TRUNCATE`.

Useful when you need write access (for support intervention, data fixes) but want to prevent accidental schema drift.

## Time Windows

Grants are only active within their time window:

- Before `starts_at`: connection refused
- Between `starts_at` and `expires_at`: access granted
- After `expires_at`: connection refused

This is useful for:
- Support engineers who need temporary access
- Contractors with limited engagement periods
- Scheduled maintenance windows

## Quotas

### Query Quota

Limit the number of queries a grant can execute:

```json
{ "max_query_counts": 100 }
```

When exceeded, subsequent queries return an error.

### Data Transfer Quota

Limit the volume of data returned through the proxy:

```json
{ "max_bytes_transferred": 104857600 }
```

When exceeded, subsequent queries return an error. The byte counter accumulates response sizes from the upstream database.

Counters (`query_count`, `bytes_transferred`) are exposed on the grant object so admins can see usage in real time.

## Revoking Grants

Manually revoke a grant before expiration:

```bash
curl -X DELETE http://localhost:4200/api/v1/grants/$GRANT_UID \
  -H "Authorization: Bearer $TOKEN"
```

The grant record is preserved for audit (with `revoked_at` and `revoked_by` populated); only new connections are refused.

## Listing Grants

List all grants:

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/grants
```

Filter by user, database, or active state:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/grants?user_id=$USER_UID&active_only=true"

curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/grants?database_id=$DB_UID"
```

Connectors only see their own grants; admins and viewers see all.

## Audit Trail

All grant operations are logged in the audit log:
- Grant creation (who granted, to whom, which database, what controls and quotas)
- Grant revocation (who revoked, when)

View the audit log:

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/audit
```
