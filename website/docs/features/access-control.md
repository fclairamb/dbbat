---
sidebar_position: 1
---

# Access Control

DBBat provides fine-grained access control through grants. A grant gives a user permission to access a specific database for a limited time with optional quotas.

## Creating a Grant

```bash
curl -u admin:admin -X POST http://localhost:8080/api/grants \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 2,
    "database_id": 1,
    "access_level": "read",
    "starts_at": "2024-01-15T09:00:00Z",
    "expires_at": "2024-01-15T18:00:00Z",
    "max_queries": 1000,
    "max_bytes": 104857600
  }'
```

## Grant Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `user_id` | integer | ID of the user | Yes |
| `database_id` | integer | ID of the database configuration | Yes |
| `access_level` | string | `read` or `write` | Yes |
| `starts_at` | datetime | When the grant becomes active | Yes |
| `expires_at` | datetime | When the grant expires | Yes |
| `max_queries` | integer | Maximum number of queries allowed | No |
| `max_bytes` | integer | Maximum bytes transferred | No |

## Access Levels

### Read

Read-only access prevents any write operations:
- `SELECT` queries are allowed
- `INSERT`, `UPDATE`, `DELETE`, `DROP`, `TRUNCATE`, `CREATE`, `ALTER`, `GRANT`, `REVOKE` are blocked

### Write

Full read/write access allows all query types.

## Time Windows

Grants are only active within their time window:
- Before `starts_at`: Connection refused
- Between `starts_at` and `expires_at`: Access granted
- After `expires_at`: Connection refused

This is useful for:
- Support engineers who need temporary access
- Contractors with limited engagement periods
- Scheduled maintenance windows

## Quotas

### Query Quota

Limit the number of queries a user can execute:

```json
{
  "max_queries": 100
}
```

When exceeded, subsequent queries return an error.

### Data Transfer Quota

Limit the amount of data transferred:

```json
{
  "max_bytes": 104857600
}
```

When exceeded, subsequent queries return an error. This is calculated based on the response size from the database.

## Revoking Grants

Manually revoke a grant before expiration:

```bash
curl -u admin:admin -X DELETE http://localhost:8080/api/grants/1
```

## Listing Grants

List all grants:

```bash
curl -u admin:admin http://localhost:8080/api/grants
```

Filter by user:

```bash
curl -u admin:admin "http://localhost:8080/api/grants?user_id=2"
```

Filter by database:

```bash
curl -u admin:admin "http://localhost:8080/api/grants?database_id=1"
```

## Audit Trail

All grant operations are logged in the audit log:
- Grant creation (who granted, to whom, which database)
- Grant revocation (who revoked, when)

View the audit log:

```bash
curl -u admin:admin http://localhost:8080/api/audit
```
