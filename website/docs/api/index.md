---
sidebar_position: 1
---

# API Reference

DBBat provides a comprehensive REST API for managing users, databases, grants, and viewing observability data.

## Base URL

All API endpoints are versioned under `/api/v1/`:

```
http://localhost:8080/api/v1
```

## OpenAPI Specification

The full OpenAPI 3.0 specification is available at:

- **Spec file**: `GET /api/openapi.yml`
- **Interactive docs**: `GET /api/docs` (Swagger UI)

## Authentication

The API supports two authentication methods:

### Basic Auth

HTTP Basic Authentication using username and password:

```bash
curl -u username:password http://localhost:8080/api/v1/users
```

### Bearer Token

API key or web session token authentication:

```bash
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/users
```

:::note
API keys cannot create or revoke other API keys (security restriction) - these operations require Basic Auth or a web session token.
:::

## Roles

Users have one or more roles that determine their access:

| Role | Description |
|------|-------------|
| `admin` | Full access to all resources and operations |
| `viewer` | Read-only access to observability data (connections, queries, audit) |
| `connector` | Can only access databases they have active grants for |

## Password Change Requirement

Users must change their initial password before logging in. Login attempts from users who haven't changed their password will receive a `403 Forbidden` response with error code `password_change_required`.

Users must then call `PUT /auth/password` with their username and credentials to change their password before they can log in.

## Rate Limiting

### General Rate Limiting

All authenticated endpoints are rate-limited. Response headers include:

| Header | Description |
|--------|-------------|
| `X-RateLimit-Limit` | Maximum requests per minute |
| `X-RateLimit-Remaining` | Remaining requests in current window |
| `X-RateLimit-Reset` | Unix timestamp when limit resets |

### Authentication Rate Limiting

Failed login attempts are rate-limited per username with exponential backoff:

| Failures | Delay |
|----------|-------|
| 1-2 | No delay |
| 3-4 | 5 seconds |
| 5-6 | 30 seconds |
| 7-9 | 2 minutes |
| 10+ | 5 minutes |

Rate-limited authentication attempts receive a `429 Too Many Requests` response with error code `auth_rate_limited`.

## Pagination

List endpoints support pagination with `limit` and `offset` parameters:

```bash
curl -u admin:password "http://localhost:8080/api/v1/queries?limit=100&offset=0"
```

| Parameter | Default | Min | Max | Description |
|-----------|---------|-----|-----|-------------|
| `limit` | 100 | 1 | 1000 | Maximum number of results |
| `offset` | 0 | 0 | - | Number of results to skip |

## Response Format

All responses are JSON. Successful responses return the requested data:

```json
{
  "uid": "550e8400-e29b-41d4-a716-446655440000",
  "username": "admin",
  "roles": ["admin"],
  "created_at": "2024-01-01T00:00:00Z"
}
```

Error responses include an error message:

```json
{
  "error": "user not found"
}
```

---

## Health & Version

### Health Check

```
GET /api/v1/health
```

Returns the health status of the API server and database connection. No authentication required.

**Response:**

```json
{
  "status": "healthy"
}
```

### Version Info

```
GET /api/v1/version
```

Returns API version and build information. No authentication required.

**Response:**

```json
{
  "api_version": "v1",
  "build_version": "1.2.3",
  "build_commit": "abc1234",
  "build_time": "2024-01-09T12:00:00Z"
}
```

---

## Authentication Endpoints

### Login

```
POST /api/v1/auth/login
```

Authenticates with username/password and creates a short-lived web session token (1 hour).

:::warning
Users who haven't changed their initial password will receive a 403 error. They must first change their password using `PUT /auth/password`.
:::

**Request:**

```json
{
  "username": "admin",
  "password": "secretpassword"
}
```

**Response:**

```json
{
  "token": "web_abc123...",
  "expires_at": "2024-01-01T01:00:00Z",
  "user": {
    "uid": "550e8400-e29b-41d4-a716-446655440000",
    "username": "admin",
    "roles": ["admin"],
    "password_change_required": false
  }
}
```

### Logout

```
POST /api/v1/auth/logout
```

Revokes the current web session token. Requires authentication.

**Response:** `204 No Content`

### Get Current User

```
GET /api/v1/auth/me
```

Returns information about the currently authenticated user and their session.

**Response:**

```json
{
  "uid": "550e8400-e29b-41d4-a716-446655440000",
  "username": "admin",
  "roles": ["admin"],
  "password_change_required": false,
  "session": {
    "expires_at": "2024-01-01T01:00:00Z",
    "created_at": "2024-01-01T00:00:00Z"
  }
}
```

### Change Password (Pre-login)

```
PUT /api/v1/auth/password
```

Allows users to change their password before logging in. No authentication required - validates username/current_password.

**Request:**

```json
{
  "username": "newuser",
  "current_password": "temporary123",
  "new_password": "newSecurePassword123!"
}
```

**Response:**

```json
{
  "message": "Password changed successfully"
}
```

---

## Users

### Create User

```
POST /api/v1/users
```

Creates a new user account. **Requires admin role.**

**Request:**

```json
{
  "username": "developer",
  "password": "initialPassword123",
  "roles": ["connector"]
}
```

**Response:**

```json
{
  "uid": "550e8400-e29b-41d4-a716-446655440000",
  "username": "developer",
  "roles": ["connector"],
  "rate_limit_exempt": false,
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z"
}
```

### List Users

```
GET /api/v1/users
```

Returns a list of users. Admins see all users; non-admins see only themselves.

**Response:**

```json
{
  "users": [
    {
      "uid": "550e8400-e29b-41d4-a716-446655440000",
      "username": "admin",
      "roles": ["admin"],
      "rate_limit_exempt": true,
      "created_at": "2024-01-01T00:00:00Z",
      "updated_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

### Get User

```
GET /api/v1/users/:uid
```

Retrieves a specific user by their UID.

### Update User

```
PUT /api/v1/users/:uid
```

Updates a user account.

- Non-admins can only update their own password
- Non-admins cannot change roles
- API keys cannot change passwords (requires Basic Auth)

**Request:**

```json
{
  "password": "newPassword123",
  "roles": ["admin", "connector"]
}
```

### Delete User

```
DELETE /api/v1/users/:uid
```

Deletes a user account. **Requires admin role.** Cannot delete your own account.

### Change Password (Authenticated)

```
PUT /api/v1/users/:uid/password
```

Changes a user's password. Requires re-authentication via username/password in the request body.

**Request:**

```json
{
  "username": "admin",
  "current_password": "oldPassword",
  "new_password": "newSecurePassword123!"
}
```

---

## API Keys

### Create API Key

```
POST /api/v1/keys
```

Creates a new API key for the authenticated user. Requires Web Session or Basic Auth (API keys cannot create other API keys).

:::warning
The full API key is only returned once in this response. Store it securely as it cannot be retrieved later.
:::

**Request:**

```json
{
  "name": "CI/CD Pipeline",
  "expires_at": "2025-01-01T00:00:00Z"
}
```

**Response:**

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "CI/CD Pipeline",
  "key": "dbb_abc123xyz...",
  "key_prefix": "dbb_abc1",
  "expires_at": "2025-01-01T00:00:00Z",
  "created_at": "2024-01-01T00:00:00Z"
}
```

### List API Keys

```
GET /api/v1/keys
```

Returns a list of API keys. Non-admins can only see their own keys.

**Query Parameters:**

| Parameter | Description |
|-----------|-------------|
| `user_id` | Filter by user UID (admin only) |
| `include_all` | Include revoked and expired keys (default: false) |

### Get API Key

```
GET /api/v1/keys/:id
```

Retrieves a specific API key. Non-admins can only see their own keys.

### Revoke API Key

```
DELETE /api/v1/keys/:id
```

Revokes an API key. Requires Web Session or Basic Auth (API keys cannot revoke API keys).

**Response:** `204 No Content`

---

## Databases

### Create Database

```
POST /api/v1/databases
```

Creates a new database configuration. **Requires admin role.**

**Request:**

```json
{
  "name": "production-main",
  "description": "Main production database",
  "host": "db.example.com",
  "port": 5432,
  "database_name": "myapp",
  "username": "app_user",
  "password": "dbPassword123",
  "ssl_mode": "require"
}
```

**Response:**

```json
{
  "uid": "550e8400-e29b-41d4-a716-446655440000",
  "name": "production-main",
  "description": "Main production database",
  "host": "db.example.com",
  "port": 5432,
  "database_name": "myapp",
  "username": "app_user",
  "ssl_mode": "require",
  "created_by": "660e8400-e29b-41d4-a716-446655440000"
}
```

### List Databases

```
GET /api/v1/databases
```

Returns a list of database configurations. Response varies by role:

| Role | Response |
|------|----------|
| Admin | Full details (host, port, database_name, username, ssl_mode) |
| Viewer | Limited info (uid, name, description) |
| Connector | Only databases with active grants (limited info) |

### Get Database

```
GET /api/v1/databases/:uid
```

Retrieves a specific database configuration. Response varies by role (same as list).

### Update Database

```
PUT /api/v1/databases/:uid
```

Updates a database configuration. **Requires admin role.**

**Request:**

```json
{
  "description": "Updated description",
  "host": "new-db.example.com",
  "password": "newDbPassword123"
}
```

### Delete Database

```
DELETE /api/v1/databases/:uid
```

Deletes a database configuration. **Requires admin role.**

---

## Grants

### Create Grant

```
POST /api/v1/grants
```

Creates a new access grant for a user to a database. **Requires admin role.**

**Request:**

```json
{
  "user_id": "550e8400-e29b-41d4-a716-446655440000",
  "database_id": "660e8400-e29b-41d4-a716-446655440000",
  "access_level": "read",
  "starts_at": "2024-01-01T00:00:00Z",
  "expires_at": "2024-12-31T23:59:59Z",
  "max_query_counts": 10000,
  "max_bytes_transferred": 1073741824
}
```

**Response:**

```json
{
  "uid": "770e8400-e29b-41d4-a716-446655440000",
  "user_id": "550e8400-e29b-41d4-a716-446655440000",
  "database_id": "660e8400-e29b-41d4-a716-446655440000",
  "access_level": "read",
  "granted_by": "880e8400-e29b-41d4-a716-446655440000",
  "starts_at": "2024-01-01T00:00:00Z",
  "expires_at": "2024-12-31T23:59:59Z",
  "revoked_at": null,
  "revoked_by": null,
  "max_query_counts": 10000,
  "max_bytes_transferred": 1073741824,
  "query_count": 0,
  "bytes_transferred": 0,
  "created_at": "2024-01-01T00:00:00Z"
}
```

### List Grants

```
GET /api/v1/grants
```

Returns a list of access grants. Connectors can only see their own grants.

**Query Parameters:**

| Parameter | Description |
|-----------|-------------|
| `user_id` | Filter by user UID |
| `database_id` | Filter by database UID |
| `active_only` | Only return active (non-revoked, within time window) grants |

### Get Grant

```
GET /api/v1/grants/:uid
```

Retrieves a specific access grant. Connectors can only see their own grants.

### Revoke Grant

```
DELETE /api/v1/grants/:uid
```

Revokes an access grant. **Requires admin role.**

---

## Connections

### List Connections

```
GET /api/v1/connections
```

Returns a list of proxy connections. Connectors can only see their own connections.

**Query Parameters:**

| Parameter | Description |
|-----------|-------------|
| `user_id` | Filter by user UID |
| `database_id` | Filter by database UID |
| `limit` | Maximum results (default: 100, max: 1000) |
| `offset` | Skip results for pagination |

**Response:**

```json
{
  "connections": [
    {
      "uid": "550e8400-e29b-41d4-a716-446655440000",
      "user_id": "660e8400-e29b-41d4-a716-446655440000",
      "database_id": "770e8400-e29b-41d4-a716-446655440000",
      "source_ip": "192.168.1.100",
      "connected_at": "2024-01-01T10:00:00Z",
      "last_activity_at": "2024-01-01T10:30:00Z",
      "disconnected_at": "2024-01-01T11:00:00Z",
      "queries": 150,
      "bytes_transferred": 1048576
    }
  ]
}
```

---

## Queries

### List Queries

```
GET /api/v1/queries
```

Returns a list of executed queries. **Requires admin or viewer role.**

**Query Parameters:**

| Parameter | Description |
|-----------|-------------|
| `connection_id` | Filter by connection UID |
| `user_id` | Filter by user UID |
| `database_id` | Filter by database UID |
| `start_time` | Filter by start time (RFC3339 format) |
| `end_time` | Filter by end time (RFC3339 format) |
| `limit` | Maximum results (default: 100, max: 1000) |
| `offset` | Skip results for pagination |

**Response:**

```json
{
  "queries": [
    {
      "uid": "550e8400-e29b-41d4-a716-446655440000",
      "connection_id": "660e8400-e29b-41d4-a716-446655440000",
      "sql_text": "SELECT * FROM users WHERE id = $1",
      "parameters": {
        "values": ["123"],
        "format_codes": [0],
        "type_oids": [23]
      },
      "executed_at": "2024-01-01T10:15:00Z",
      "duration_ms": 12.5,
      "rows_affected": 1,
      "error": null
    }
  ]
}
```

### Get Query

```
GET /api/v1/queries/:uid
```

Retrieves a specific query without its result rows. **Requires admin or viewer role.**

Use `GET /queries/:uid/rows` to retrieve the result rows.

### Get Query Rows

```
GET /api/v1/queries/:uid/rows
```

Retrieves paginated result rows for a specific query. **Requires admin or viewer role.**

Uses cursor-based pagination with limits per request:
- Maximum 1000 rows per response
- Maximum 1MB of row data per response

The response stops at whichever limit is reached first.

**Query Parameters:**

| Parameter | Description |
|-----------|-------------|
| `cursor` | Pagination cursor from previous response |
| `limit` | Maximum rows (default: 100, max: 1000) |

**Response:**

```json
{
  "rows": [
    {
      "row_number": 0,
      "row_data": {"id": 1, "name": "Alice", "email": "alice@example.com"},
      "row_size_bytes": 64
    },
    {
      "row_number": 1,
      "row_data": {"id": 2, "name": "Bob", "email": "bob@example.com"},
      "row_size_bytes": 62
    }
  ],
  "next_cursor": "eyJvZmZzZXQiOjEwMH0=",
  "has_more": true,
  "total_rows": 500
}
```

---

## Audit

### List Audit Events

```
GET /api/v1/audit
```

Returns a list of audit log events. **Requires admin or viewer role.**

**Query Parameters:**

| Parameter | Description |
|-----------|-------------|
| `event_type` | Filter by event type (e.g., `user.created`, `grant.revoked`) |
| `user_id` | Filter by user UID (the user being acted upon) |
| `performed_by` | Filter by performer UID (the user performing the action) |
| `start_time` | Filter by start time (RFC3339 format) |
| `end_time` | Filter by end time (RFC3339 format) |
| `limit` | Maximum results (default: 100, max: 1000) |
| `offset` | Skip results for pagination |

**Response:**

```json
{
  "audit_events": [
    {
      "uid": "550e8400-e29b-41d4-a716-446655440000",
      "event_type": "user.created",
      "user_id": "660e8400-e29b-41d4-a716-446655440000",
      "performed_by": "770e8400-e29b-41d4-a716-446655440000",
      "details": {
        "username": "newuser",
        "roles": ["connector"]
      },
      "created_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

---

## Error Responses

### Standard Error

```json
{
  "error": "error message here"
}
```

### Rate Limit Error

```json
{
  "error": "rate_limit_exceeded",
  "message": "Too many requests. Please retry after 60 seconds.",
  "retry_after": 60
}
```

### Authentication Rate Limit Error

```json
{
  "error": "auth_rate_limited",
  "message": "Too many failed login attempts. Try again in 30 seconds.",
  "retry_after": 30
}
```

### Password Change Required Error

```json
{
  "error": "password_change_required",
  "message": "You must change your password before accessing the API"
}
```

### Weak Password Error

```json
{
  "error": "weak_password",
  "message": "Password does not meet security requirements"
}
```
