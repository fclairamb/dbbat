---
sidebar_position: 3
---

# User Management

DBBat maintains its own user database, separate from target database users. This separation provides:

- Independent credentials for proxy access (one DBBat login, many target databases)
- Central user management across PostgreSQL, Oracle, and MySQL/MariaDB targets
- Audit trail of user actions

## Roles

Each user carries one or more roles. Roles are **additive**.

| Role | Description |
|------|-------------|
| `admin` | Full access to all resources and operations |
| `viewer` | Read-only access to observability data (connections, queries, audit) |
| `connector` | Can only connect through the proxy to databases with active grants |

A user can have multiple roles (e.g. `["admin", "connector"]` so an admin can also connect through the proxy).

## Creating Users

Create a new user via the REST API. **Admin role required.**

```bash
curl -X POST http://localhost:4200/api/v1/users \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "username": "developer",
    "password": "TempPassword123!",
    "roles": ["connector"]
  }'
```

## User Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `username` | string | Unique username | Yes |
| `password` | string | Initial user password (hashed with Argon2id) | Yes |
| `roles` | array | Any combination of `admin`, `viewer`, `connector` | No (default: `["connector"]`) |

The user's initial password must be **changed before first login** — see "Initial password change" below.

## Listing Users

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/users
```

Admins see all users; non-admins see only themselves.

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

## Updating Users

```bash
curl -X PUT http://localhost:4200/api/v1/users/$USER_UID \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "roles": ["admin", "connector"] }'
```

- Non-admins can only update their own password
- Non-admins cannot change roles
- API keys cannot change passwords (Basic Auth or web session required)

## Changing Passwords

Authenticated password change (the user re-supplies their current credentials in the body):

```bash
curl -X PUT http://localhost:4200/api/v1/users/$USER_UID/password \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "username": "developer",
    "current_password": "TempPassword123!",
    "new_password": "EvenBetterPassword456!"
  }'
```

## Initial Password Change

Newly created users (and the default `admin`) must change their password **before logging in**. Login attempts return `403 password_change_required`. The pre-login change endpoint accepts the username and current password without an auth header:

```bash
curl -X PUT http://localhost:4200/api/v1/auth/password \
  -H "Content-Type: application/json" \
  -d '{
    "username": "developer",
    "current_password": "TempPassword123!",
    "new_password": "EvenBetterPassword456!"
  }'
```

## API Keys

For programmatic access, create a long-lived API key (`dbb_…`) instead of using a username/password.

```bash
curl -X POST http://localhost:4200/api/v1/keys \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "name": "ci-pipeline", "expires_at": "2025-12-31T23:59:59Z" }'
```

The full key is **only returned once**. Store it securely.

API keys carry their owner's roles, but with two intentional restrictions:

- They **cannot create** other API keys.
- They **cannot revoke** API keys.

These operations require Basic Auth or a web session token. This prevents a leaked API key from being used to bootstrap persistent backdoor access.

API keys can also be used as the password against the proxy listeners (PostgreSQL, MySQL, Oracle) — DBBat detects the `dbb_` prefix and verifies it as a key.

## Slack OAuth (optional)

When configured, users can sign in with their Slack workspace account. New users are auto-provisioned by default with the `connector` role. Configurable via:

- `slack_auth.client_id` / `slack_auth.client_secret`
- `slack_auth.team_id` (optional — restrict to one workspace)
- `slack_auth.auto_create_users` (default `true`)
- `slack_auth.default_role` (default `connector`)

## Deleting Users

```bash
curl -X DELETE http://localhost:4200/api/v1/users/$USER_UID \
  -H "Authorization: Bearer $TOKEN"
```

Deleting a user:
- Revokes all their active grants
- Preserves their query, connection, and audit history
- Prevents any future connections

You cannot delete your own account.

## Default Admin

On first startup, DBBat creates:
- **Username**: `admin`
- **Password**: `admin`

The password is flagged as requiring change — the very first login will fail with `403 password_change_required` until you call `PUT /api/v1/auth/password` to set a real password.
