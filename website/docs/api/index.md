---
sidebar_position: 1
---

# API Overview

DBBat provides a REST API for managing users, databases, grants, and viewing observability data.

## Base URL

```
http://localhost:8080/api
```

## Authentication

All API endpoints require Basic Authentication using a DBBat user's credentials.

```bash
curl -u username:password http://localhost:8080/api/users
```

Admin endpoints require an admin user.

## OpenAPI Specification

The full OpenAPI 3.0 specification is available at:

- **Spec file**: `GET /api/openapi.yml`
- **Interactive docs**: `GET /api/docs` (Swagger UI)

## Endpoints Summary

### Users (Admin only)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/users` | Create user |
| `GET` | `/api/users` | List users |
| `GET` | `/api/users/:id` | Get user |
| `PUT` | `/api/users/:id` | Update user |
| `DELETE` | `/api/users/:id` | Delete user |

### Databases (Admin only)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/databases` | Create database configuration |
| `GET` | `/api/databases` | List databases |
| `GET` | `/api/databases/:id` | Get database |
| `PUT` | `/api/databases/:id` | Update database |
| `DELETE` | `/api/databases/:id` | Delete database |

### Grants

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/grants` | Create grant (admin) |
| `GET` | `/api/grants` | List grants |
| `GET` | `/api/grants/:id` | Get grant |
| `DELETE` | `/api/grants/:id` | Revoke grant (admin) |

### Observability

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/connections` | List connections |
| `GET` | `/api/queries` | List queries |
| `GET` | `/api/queries/:id` | Get query with results |
| `GET` | `/api/audit` | View audit log (admin) |

### Health

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/health` | Health check |

## Response Format

All responses are JSON. Successful responses return the requested data:

```json
{
  "id": 1,
  "username": "admin",
  "is_admin": true,
  "created_at": "2024-01-01T00:00:00Z"
}
```

Error responses include a message:

```json
{
  "error": "user not found"
}
```

## Pagination

List endpoints support pagination:

```bash
curl -u admin:admin "http://localhost:8080/api/queries?limit=20&offset=0"
```

## Filtering

Most list endpoints support filtering via query parameters:

```bash
curl -u admin:admin "http://localhost:8080/api/queries?user_id=2&database_id=1"
curl -u admin:admin "http://localhost:8080/api/grants?user_id=2"
```

## Rate Limiting

The API includes rate limiting on authentication endpoints to prevent brute-force attacks.
