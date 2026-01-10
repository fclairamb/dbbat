---
sidebar_position: 2
---

# Database Configuration

Target databases are configured through the REST API. Each database configuration maps a DBBat database name to a target PostgreSQL server.

## Creating a Database Configuration

```bash
curl -u admin:admin -X POST http://localhost:8080/api/databases \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production",
    "host": "prod-db.example.com",
    "port": 5432,
    "username": "app_user",
    "password": "secret",
    "database": "myapp",
    "ssl_mode": "require",
    "description": "Production database"
  }'
```

## Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `name` | string | DBBat database name (used in connection string) | Yes |
| `host` | string | Target PostgreSQL host | Yes |
| `port` | integer | Target PostgreSQL port | Yes (default: 5432) |
| `username` | string | Target database username | Yes |
| `password` | string | Target database password (encrypted at rest) | Yes |
| `database` | string | Target database name | Yes |
| `ssl_mode` | string | SSL mode for target connection | No (default: `disable`) |
| `description` | string | Human-readable description | No |

## SSL Modes

- `disable` - No SSL connection
- `require` - Use SSL, don't verify certificate
- `verify-ca` - Verify server certificate against CA
- `verify-full` - Verify certificate and hostname match

## Listing Databases

```bash
curl -u admin:admin http://localhost:8080/api/databases
```

Response:

```json
[
  {
    "id": 1,
    "name": "production",
    "host": "prod-db.example.com",
    "port": 5432,
    "username": "app_user",
    "database": "myapp",
    "ssl_mode": "require",
    "description": "Production database",
    "created_at": "2024-01-15T10:30:00Z"
  }
]
```

Note: Passwords are never returned in API responses.

## Updating a Database

```bash
curl -u admin:admin -X PUT http://localhost:8080/api/databases/1 \
  -H "Content-Type: application/json" \
  -d '{
    "description": "Updated description",
    "password": "new-secret"
  }'
```

Only provide fields you want to update.

## Deleting a Database

```bash
curl -u admin:admin -X DELETE http://localhost:8080/api/databases/1
```

Deleting a database configuration will:
- Prevent new connections to that database
- Not affect existing active connections
- Not delete any logged queries or connections

## Connection Flow

When a user connects with `database=production`:

1. DBBat looks up the database configuration named `production`
2. Decrypts the stored credentials
3. Connects to the target server using those credentials
4. Proxies all queries between client and target
