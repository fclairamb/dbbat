---
sidebar_position: 3
---

# User Management

DBBat maintains its own user database, separate from target database users. This separation provides:

- Independent credentials for proxy access
- Central user management across multiple databases
- Audit trail of user actions

## Creating Users

Create a new user via the REST API:

```bash
curl -u admin:admin -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{
    "username": "analyst",
    "password": "secure-password",
    "is_admin": false
  }'
```

## User Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `username` | string | Unique username | Yes |
| `password` | string | User password (hashed with Argon2id) | Yes |
| `is_admin` | boolean | Whether user has admin privileges | No (default: false) |

## Admin vs Regular Users

### Regular Users

- Can connect through the proxy (with valid grants)
- Can view their own connections and queries
- Cannot manage other users, databases, or grants

### Admin Users

- All regular user capabilities
- Create/modify/delete users
- Create/modify/delete database configurations
- Create/revoke grants
- View all connections, queries, and audit logs

## Listing Users

```bash
curl -u admin:admin http://localhost:8080/api/users
```

Response:

```json
[
  {
    "id": 1,
    "username": "admin",
    "is_admin": true,
    "created_at": "2024-01-01T00:00:00Z"
  },
  {
    "id": 2,
    "username": "analyst",
    "is_admin": false,
    "created_at": "2024-01-15T10:00:00Z"
  }
]
```

## Updating Users

Update user details (admin only, or self for password):

```bash
curl -u admin:admin -X PUT http://localhost:8080/api/users/2 \
  -H "Content-Type: application/json" \
  -d '{
    "password": "new-secure-password"
  }'
```

## Deleting Users

```bash
curl -u admin:admin -X DELETE http://localhost:8080/api/users/2
```

Deleting a user:
- Revokes all their active grants
- Preserves their query and connection history for audit
- Prevents any future connections

## Password Requirements

While DBBat doesn't enforce specific password policies, we recommend:

- Minimum 12 characters
- Mix of letters, numbers, and symbols
- Unique passwords per user
- Regular rotation for sensitive environments

## Default Admin

On first startup, DBBat creates:
- **Username**: `admin`
- **Password**: `admin`

**Important**: Change this password immediately:

```bash
curl -u admin:admin -X PUT http://localhost:8080/api/users/1 \
  -H "Content-Type: application/json" \
  -d '{"password": "your-secure-password"}'
```
