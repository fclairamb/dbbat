---
sidebar_position: 2
---

# Database Configuration

Target databases are configured through the REST API. Each entry maps a DBBat database name to a target server (PostgreSQL, Oracle, MySQL, or MariaDB).

## Creating a Database Configuration

### PostgreSQL

```bash
curl -X POST http://localhost:4200/api/v1/databases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production",
    "description": "Production PostgreSQL",
    "protocol": "postgresql",
    "host": "prod-db.example.com",
    "port": 5432,
    "database_name": "myapp",
    "username": "app_user",
    "password": "secret",
    "ssl_mode": "require"
  }'
```

### Oracle

```bash
curl -X POST http://localhost:4200/api/v1/databases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "orcl",
    "description": "Oracle 19c",
    "protocol": "oracle",
    "host": "oracle.example.com",
    "port": 1521,
    "database_name": "ORCL",
    "oracle_service_name": "ORCL",
    "username": "scott",
    "password": "tiger",
    "ssl_mode": "disable"
  }'
```

`oracle_service_name` is what TNS clients use to route to this entry. It can match `database_name` or be different (e.g. for PDB names).

### MySQL / MariaDB

```bash
curl -X POST http://localhost:4200/api/v1/databases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "shop",
    "description": "Production MySQL",
    "protocol": "mysql",
    "host": "mysql.example.com",
    "port": 3306,
    "database_name": "shop",
    "username": "app_user",
    "password": "secret",
    "ssl_mode": "prefer"
  }'
```

For MariaDB, set `"protocol": "mariadb"`. Both share the same listener and proxy code path; the protocol field controls UI labelling and default-port hints.

## Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `name` | string | DBBat database name (used by clients in their connection string) | Yes |
| `protocol` | enum | `postgresql`, `oracle`, `mysql`, `mariadb` | No (default: `postgresql`) |
| `host` | string | Target database host | Yes |
| `port` | integer | Target database port. Suggested defaults: 5432 / 1521 / 3306. | Yes |
| `database_name` | string | Target database name (or PDB name for Oracle) | Yes (PG/MySQL); recommended (Oracle) |
| `username` | string | Target database username | Yes |
| `password` | string | Target database password (encrypted at rest) | Yes |
| `ssl_mode` | string | SSL mode for the upstream connection | No (default: `prefer`) |
| `oracle_service_name` | string | Oracle SERVICE_NAME — used to route TNS connects | Recommended for Oracle |
| `description` | string | Human-readable description | No |

## SSL Modes

These follow the libpq convention and apply to the **upstream** connection:

- `disable` — No SSL
- `prefer` — Try SSL, fall back to plain (default)
- `require` — Require SSL, don't verify certificate
- `verify-ca` — Verify server certificate against CA
- `verify-full` — Verify certificate and hostname match

Client-side TLS for the proxy listeners is configured separately (e.g. `DBB_MYSQL_TLS_*` for the MySQL listener).

## Listing Databases

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/databases
```

Response visibility depends on the caller's role:

| Role | What they see |
|------|---------------|
| Admin | Full details (host, port, database_name, username, ssl_mode, protocol, oracle_service_name) |
| Viewer | Limited (uid, name, description) |
| Connector | Only databases they have an active grant for (limited fields) |

Passwords are **never** returned in any response.

## Updating a Database

```bash
curl -X PUT http://localhost:4200/api/v1/databases/$DB_UID \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "description": "Updated description",
    "password": "new-secret"
  }'
```

Provide only the fields you want to update. Changing `password` re-encrypts the credential.

## Deleting a Database

```bash
curl -X DELETE http://localhost:4200/api/v1/databases/$DB_UID \
  -H "Authorization: Bearer $TOKEN"
```

Deleting a database configuration:

- Prevents new connections to that database
- Does not affect existing active connections
- Preserves all logged queries and connection history (for audit)

## Connection Flow

When a user connects with `database=production`:

1. **PostgreSQL / MySQL**: DBBat looks up the entry by `name` (the database name in the client's connection string).
2. **Oracle**: DBBat matches the TNS connect descriptor's `SERVICE_NAME` against `oracle_service_name` (falls back to `name`).
3. DBBat decrypts the stored credentials.
4. DBBat verifies the user has an active, non-revoked grant for this database.
5. DBBat connects to the upstream using the stored credentials.
6. DBBat proxies all subsequent queries between client and target, logging everything.

## Storage-DSN Collision Warning

DBBat warns at startup if a configured target's `host:port/database_name` matches the DBBat storage DSN. Allowing developers to proxy *into* DBBat's own store is a privilege-escalation vector — keep them on separate databases (preferably separate clusters).
