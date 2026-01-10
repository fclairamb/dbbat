# DBBat REST API

This document provides a quick reference for the DBBat REST API. For the complete specification, see the [OpenAPI documentation](/api/docs).

## Base URL

All API endpoints are versioned under `/api/v1/`.

## Authentication

### Authentication Methods

| Method | Header | Description |
|--------|--------|-------------|
| Basic Auth | `Authorization: Basic <base64(user:pass)>` | Username/password authentication |
| Bearer Token | `Authorization: Bearer <token>` | Web session or API key |

### Token Types

| Type | Prefix | Duration | Use Case |
|------|--------|----------|----------|
| Web Session | `web_` | 1 hour | Frontend login |
| API Key | `dbb_` | Configurable | Programmatic access |

### Getting Started

1. **Initial Setup**: Login with default admin credentials (`admin`/`admin`)
2. **Change Password**: Required before any other operations
3. **Create API Key**: For programmatic access (web sessions or basic auth only)
4. **Use API Key**: For all subsequent management operations

```bash
# 1. Change the default admin password (required first)
curl -X PUT http://localhost:8080/api/v1/auth/password \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "current_password": "admin", "new_password": "NewSecurePassword123!"}'

# 2. Login to get a web session
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "NewSecurePassword123!"}' | jq -r '.token')

# 3. Create an API key for programmatic access
API_KEY=$(curl -s -X POST http://localhost:8080/api/v1/keys \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-api-key"}' | jq -r '.key')

# 4. Use the API key for subsequent requests
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/v1/users
```

### API Key Restrictions

API keys **cannot**:
- Create other API keys
- Revoke API keys

These operations require a web session token or basic auth.

## Entity Identifiers

All entities use **UUIDs** as their primary identifier:

| Entity | Field | Example |
|--------|-------|---------|
| User | `uid` | `550e8400-e29b-41d4-a716-446655440000` |
| Database | `uid` | `6ba7b810-9dad-11d1-80b4-00c04fd430c8` |
| Grant | `uid` | `6ba7b811-9dad-11d1-80b4-00c04fd430c8` |
| Connection | `uid` | `6ba7b812-9dad-11d1-80b4-00c04fd430c8` |
| Query | `uid` | `6ba7b813-9dad-11d1-80b4-00c04fd430c8` |
| API Key | `id` | `6ba7b814-9dad-11d1-80b4-00c04fd430c8` |

## Endpoints Summary

### Health & Version
| Method | Endpoint | Description | Auth |
|--------|----------|-------------|------|
| GET | `/health` | Health check | No |
| GET | `/version` | Version info | No |

### Authentication
| Method | Endpoint | Description | Auth |
|--------|----------|-------------|------|
| POST | `/auth/login` | Login (returns web session) | No |
| POST | `/auth/logout` | Logout (revokes session) | Yes |
| GET | `/auth/me` | Current user info | Yes |
| PUT | `/auth/password` | Change password (pre-login) | No |

### Users
| Method | Endpoint | Description | Auth | Role |
|--------|----------|-------------|------|------|
| POST | `/users` | Create user | Yes | Admin |
| GET | `/users` | List users | Yes | Any |
| GET | `/users/{uid}` | Get user | Yes | Any |
| PUT | `/users/{uid}` | Update user | Yes | Admin* |
| DELETE | `/users/{uid}` | Delete user | Yes | Admin |
| PUT | `/users/{uid}/password` | Change password | No** | Any |

*Non-admins can only update their own password
**Requires username/password in request body for re-authentication

### Databases
| Method | Endpoint | Description | Auth | Role |
|--------|----------|-------------|------|------|
| POST | `/databases` | Create database config | Yes | Admin |
| GET | `/databases` | List databases | Yes | Any |
| GET | `/databases/{uid}` | Get database | Yes | Any |
| PUT | `/databases/{uid}` | Update database | Yes | Admin |
| DELETE | `/databases/{uid}` | Delete database | Yes | Admin |

### Grants
| Method | Endpoint | Description | Auth | Role |
|--------|----------|-------------|------|------|
| POST | `/grants` | Create grant | Yes | Admin |
| GET | `/grants` | List grants | Yes | Any |
| GET | `/grants/{uid}` | Get grant | Yes | Any |
| DELETE | `/grants/{uid}` | Revoke grant | Yes | Admin |

### API Keys
| Method | Endpoint | Description | Auth | Restriction |
|--------|----------|-------------|------|-------------|
| POST | `/keys` | Create API key | Yes | Web session or Basic Auth only |
| GET | `/keys` | List API keys | Yes | - |
| GET | `/keys/{id}` | Get API key | Yes | - |
| DELETE | `/keys/{id}` | Revoke API key | Yes | Web session or Basic Auth only |

### Observability
| Method | Endpoint | Description | Auth | Role |
|--------|----------|-------------|------|------|
| GET | `/connections` | List connections | Yes | Admin/Viewer |
| GET | `/queries` | List queries | Yes | Admin/Viewer |
| GET | `/queries/{uid}` | Get query details | Yes | Admin/Viewer |
| GET | `/queries/{uid}/rows` | Get query result rows | Yes | Admin/Viewer |
| GET | `/audit` | List audit events | Yes | Admin/Viewer |

## Query Result Rows

Query result rows are **not** included in the query listing or detail responses. They must be fetched separately using the `/queries/{uid}/rows` endpoint.

### Pagination

The rows endpoint uses cursor-based pagination with two limits:
- Maximum 1000 rows per response
- Maximum 1MB of row data per response

The response stops at whichever limit is reached first.

```bash
# Get first page of rows
curl -H "Authorization: Bearer $API_KEY" \
  "http://localhost:8080/api/v1/queries/{query_uid}/rows?limit=100"

# Response includes next_cursor for pagination
{
  "rows": [...],
  "next_cursor": "eyJyb3dfbnVtYmVyIjoxMDB9",
  "has_more": true,
  "total_rows": 500
}

# Get next page using cursor
curl -H "Authorization: Bearer $API_KEY" \
  "http://localhost:8080/api/v1/queries/{query_uid}/rows?cursor=eyJyb3dfbnVtYmVyIjoxMDB9"
```

## Common Workflows

### Create a User and Grant Access

```bash
# Create a user
USER_UID=$(curl -s -X POST http://localhost:8080/api/v1/users \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"username": "developer", "password": "TempPass123!", "roles": ["connector"]}' \
  | jq -r '.uid')

# Create a database configuration
DB_UID=$(curl -s -X POST http://localhost:8080/api/v1/databases \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production",
    "host": "db.example.com",
    "port": 5432,
    "database_name": "myapp",
    "username": "readonly_user",
    "password": "dbpassword",
    "ssl_mode": "require"
  }' | jq -r '.uid')

# Grant read access for 24 hours
curl -X POST http://localhost:8080/api/v1/grants \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"user_id\": \"$USER_UID\",
    \"database_id\": \"$DB_UID\",
    \"access_level\": \"read\",
    \"starts_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
    \"expires_at\": \"$(date -u -d '+24 hours' +%Y-%m-%dT%H:%M:%SZ)\",
    \"max_query_counts\": 1000
  }"
```

### View Query History

```bash
# List recent queries
curl -H "Authorization: Bearer $API_KEY" \
  "http://localhost:8080/api/v1/queries?limit=10"

# Get details for a specific query
curl -H "Authorization: Bearer $API_KEY" \
  "http://localhost:8080/api/v1/queries/{query_uid}"

# Get the result rows (separate request)
curl -H "Authorization: Bearer $API_KEY" \
  "http://localhost:8080/api/v1/queries/{query_uid}/rows"
```

## Rate Limiting

All authenticated endpoints are rate-limited. Response headers include:

| Header | Description |
|--------|-------------|
| `X-RateLimit-Limit` | Maximum requests per minute |
| `X-RateLimit-Remaining` | Remaining requests in current window |
| `X-RateLimit-Reset` | Unix timestamp when limit resets |

### Authentication Rate Limiting

Failed login attempts trigger exponential backoff:

| Failures | Delay |
|----------|-------|
| 1-2 | None |
| 3-4 | 5 seconds |
| 5-6 | 30 seconds |
| 7-9 | 2 minutes |
| 10+ | 5 minutes |

## Error Responses

All errors follow a consistent format:

```json
{
  "error": "error_code",
  "message": "Human-readable description"
}
```

### Common Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `unauthorized` | 401 | Missing or invalid authentication |
| `forbidden` | 403 | Insufficient permissions |
| `password_change_required` | 403 | Must change initial password |
| `not_found` | 404 | Resource not found |
| `rate_limit_exceeded` | 429 | Too many requests |
| `auth_rate_limited` | 429 | Too many failed login attempts |
| `weak_password` | 400 | Password doesn't meet requirements |

## Roles

| Role | Description |
|------|-------------|
| `admin` | Full access to all resources and operations |
| `viewer` | Read-only access to observability data |
| `connector` | Can only access databases with active grants |

Users can have multiple roles. The most permissive role applies.
