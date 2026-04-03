# Authentication Flow

## Status: Draft

## Summary

This specification clarifies the authentication flow, particularly around password changes and credential handling. The key principle is that **users cannot obtain a session token until they have changed their initial password**.

## Core Principles

### 1. No Token Before Password Change

Users with unchanged passwords (new users, password reset) **cannot login**. Attempting to login returns a special error code but **no token is created**.

```
User with password_changed=false
        │
        ▼
POST /api/v1/auth/login
        │
        ▼
┌─────────────────────────────┐
│ Error: password_change_required │
│ No token created            │
└─────────────────────────────┘
```

### 2. Credential Handling Separation

Only **two endpoints** accept username/password credentials:

| Endpoint | Auth Method | Purpose |
|----------|-------------|---------|
| `POST /api/v1/auth/login` | Username/Password (JSON body) | Obtain session token |
| `PUT /api/v1/auth/password` | Username/Password (JSON body) | Change password (before login) |

All other endpoints use **Bearer token authentication only**.

### 3. Password Change Before First Login

The password change endpoint is special - it works **without a token** for users who haven't logged in yet:

```
┌─────────────────────────────────────────────────────────────┐
│                  First-Time User Flow                        │
└─────────────────────────────────────────────────────────────┘

1. Admin creates user with temporary password
        │
        ▼
2. User attempts POST /api/v1/auth/login
        │
        ▼
3. Response: 403 { "error": "password_change_required" }
        │
        ▼
4. User calls PUT /api/v1/auth/password with:
   - username
   - current_password (temporary)
   - new_password
        │
        ▼
5. Password changed, password_changed=true
        │
        ▼
6. User calls POST /api/v1/auth/login
        │
        ▼
7. Success: token returned
```

## API Specification

### Login Endpoint

```
POST /api/v1/auth/login
Content-Type: application/json

Request:
{
    "username": "johndoe",
    "password": "secretpassword"
}

Success Response (200 OK):
{
    "token": "web_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3",
    "expires_at": "2026-01-09T13:00:00Z",
    "user": {
        "uid": "550e8400-e29b-41d4-a716-446655440000",
        "username": "johndoe",
        "roles": ["connector"],
        "password_change_required": false
    }
}

Error Response - Invalid Credentials (401 Unauthorized):
{
    "error": "invalid_credentials",
    "message": "Invalid username or password"
}

Error Response - Password Change Required (403 Forbidden):
{
    "error": "password_change_required",
    "message": "You must change your password before logging in"
}

Error Response - Rate Limited (429 Too Many Requests):
{
    "error": "auth_rate_limited",
    "message": "Too many failed login attempts. Try again later.",
    "retry_after": 30
}
```

**Key behavior:**
- Returns 403 with `password_change_required` if user hasn't changed password
- **No token is created** in this case
- User must change password first via `PUT /api/v1/auth/password`

### Password Change Endpoint (Pre-Login)

This endpoint allows users to change their password **without a token**:

```
PUT /api/v1/auth/password
Content-Type: application/json

Request:
{
    "username": "johndoe",
    "current_password": "temporarypassword",
    "new_password": "mysecurepassword123"
}

Success Response (200 OK):
{
    "message": "Password changed successfully"
}

Error Response - Invalid Credentials (401 Unauthorized):
{
    "error": "invalid_credentials",
    "message": "Invalid username or current password"
}

Error Response - Weak Password (400 Bad Request):
{
    "error": "weak_password",
    "message": "Password must be at least 8 characters"
}

Error Response - Rate Limited (429 Too Many Requests):
{
    "error": "auth_rate_limited",
    "message": "Too many failed attempts. Try again later.",
    "retry_after": 30
}
```

**Key behavior:**
- No authentication required (no token, no Basic Auth header)
- Validates username + current_password combination
- Subject to rate limiting (same as login)
- Sets `password_changed = true` on success

### Password Change Endpoint (Authenticated User)

For users who are already logged in and want to change their password. This endpoint does NOT use a Bearer token - it requires re-authentication with username/password:

```
PUT /api/v1/users/:uid/password
Content-Type: application/json

Request:
{
    "username": "johndoe",
    "current_password": "oldpassword",
    "new_password": "newpassword"
}

Success Response (200 OK):
{
    "message": "Password changed successfully"
}

Error Response - Invalid Credentials (401 Unauthorized):
{
    "error": "invalid_credentials",
    "message": "Invalid username or current password"
}

Error Response - Permission Denied (403 Forbidden):
{
    "error": "forbidden",
    "message": "You can only change your own password"
}
```

**Key behavior:**
- Does NOT use Bearer token authentication
- Requires username + current_password in request body (re-authentication)
- User can only change their own password (`:uid` must match authenticated user)
- Admin users can change any user's password (with their own admin credentials)
- Subject to rate limiting (same as login)

## Authentication States

```
┌─────────────────────────────────────────────────────────────┐
│                    User States                               │
└─────────────────────────────────────────────────────────────┘

┌──────────────────┐
│ password_changed │
│     = false      │
│                  │
│ (New user or     │
│  password reset) │
└────────┬─────────┘
         │
         │ PUT /api/v1/auth/password
         │ (username + old + new password)
         │
         ▼
┌──────────────────┐
│ password_changed │
│     = true       │
│                  │
│ (Can login)      │
└────────┬─────────┘
         │
         │ POST /api/v1/auth/login
         │ (username + password)
         │
         ▼
┌──────────────────┐
│   Authenticated  │
│   (has token)    │
│                  │
│ All APIs work    │
└──────────────────┘
```

## Endpoint Authentication Matrix

| Endpoint | No Auth | Username/Password | Web Session (`web_`) | API Key (`dbb_`) |
|----------|---------|-------------------|----------------------|------------------|
| `GET /api/v1/health` | Yes | - | - | - |
| `GET /api/v1/version` | Yes | - | - | - |
| `POST /api/v1/auth/login` | - | Yes (JSON body) | - | - |
| `PUT /api/v1/auth/password` | - | Yes (JSON body) | - | - |
| `POST /api/v1/auth/logout` | - | - | Yes | - |
| `GET /api/v1/auth/me` | - | - | Yes | Yes |
| **Users** |
| `GET /api/v1/users` | - | - | Yes | Yes |
| `POST /api/v1/users` | - | - | Yes | Yes |
| `PUT /api/v1/users/:uid` | - | - | Yes | Yes* |
| `PUT /api/v1/users/:uid/password` | - | Yes (JSON body) | - | - |
| `DELETE /api/v1/users/:uid` | - | - | Yes | Yes |
| **API Keys** |
| `POST /api/v1/keys` | - | - | Yes | No |
| `GET /api/v1/keys` | - | - | Yes | Yes |
| `DELETE /api/v1/keys/:id` | - | - | Yes | No |
| **Databases** |
| `GET /api/v1/databases` | - | - | Yes | Yes |
| `POST /api/v1/databases` | - | - | Yes | Yes |
| `PUT /api/v1/databases/:uid` | - | - | Yes | Yes |
| `DELETE /api/v1/databases/:uid` | - | - | Yes | Yes |
| **Grants** |
| `GET /api/v1/grants` | - | - | Yes | Yes |
| `POST /api/v1/grants` | - | - | Yes | Yes |
| `DELETE /api/v1/grants/:uid` | - | - | Yes | Yes |
| **Observability** |
| `GET /api/v1/connections` | - | - | Yes | Yes |
| `GET /api/v1/queries` | - | - | Yes | Yes |
| `GET /api/v1/audit` | - | - | Yes | Yes |

*API keys cannot modify the user they belong to

**Note**: All management operations (users, databases, grants) require either a Web Session or API Key. Username/password authentication is ONLY used for:
- Login (`POST /api/v1/auth/login`)
- Pre-login password change (`PUT /api/v1/auth/password`)
- Authenticated password change (`PUT /api/v1/users/:uid/password`)

## Frontend Flow

### Initial Load

```
┌─────────────────────────────────────────────────────────────┐
│                       App Load                               │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
               ┌──────────────────────────┐
               │ Check localStorage for   │
               │ session token            │
               └──────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              │                               │
        Token exists                    No token
              │                               │
              ▼                               ▼
     ┌─────────────────┐            ┌─────────────────┐
     │ GET /api/v1/auth/me │        │ Show login page │
     └─────────────────┘            └─────────────────┘
              │
     ┌────────┴────────┐
     │                 │
  200 OK            401
     │                 │
     ▼                 ▼
┌─────────────┐  ┌─────────────────┐
│ Show app    │  │ Clear token,    │
│             │  │ show login      │
└─────────────┘  └─────────────────┘
```

### Login Flow

```
┌─────────────────────────────────────────────────────────────┐
│                    Login Page                                │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
               ┌──────────────────────────┐
               │ User enters credentials  │
               │ POST /api/v1/auth/login  │
               └──────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
           200 OK            403             401
              │               │               │
              ▼               ▼               ▼
     ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
     │ Store token │  │ Show        │  │ Show error  │
     │ Redirect to │  │ password    │  │ "Invalid    │
     │ dashboard   │  │ change form │  │ credentials"│
     └─────────────┘  └─────────────┘  └─────────────┘
```

### Password Change Flow (First Login)

```
┌─────────────────────────────────────────────────────────────┐
│               Password Change Form                           │
│    (shown when login returns password_change_required)       │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
               ┌──────────────────────────┐
               │ PUT /api/v1/auth/password │
               │ username + old + new     │
               └──────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              │                               │
           200 OK                          Error
              │                               │
              ▼                               ▼
     ┌─────────────────┐            ┌─────────────────┐
     │ Show success    │            │ Show error      │
     │ Auto-login with │            │ (invalid pass,  │
     │ new password    │            │ weak password)  │
     └─────────────────┘            └─────────────────┘
```

## Audit Logging

All operations that modify data MUST record the authentication method used:

### Audit Log Fields

| Field | Description |
|-------|-------------|
| `user_id` | The user who performed the action |
| `key_id` | The API key or web session key used (NULL if username/password auth) |
| `key_type` | Type of key: `web`, `api`, or `password` |
| `action` | The action performed (e.g., `user.created`, `grant.revoked`) |
| `resource_type` | Type of resource affected |
| `resource_id` | ID of the affected resource |
| `details` | JSON with additional context |
| `ip_address` | Client IP address |
| `created_at` | Timestamp |

### Example Audit Log Entries

**User created via Web Session:**
```json
{
    "user_id": "550e8400-e29b-41d4-a716-446655440000",
    "key_id": "660e8400-e29b-41d4-a716-446655440001",
    "key_type": "web",
    "action": "user.created",
    "resource_type": "user",
    "resource_id": "770e8400-e29b-41d4-a716-446655440002",
    "details": {"username": "newuser"},
    "ip_address": "192.168.1.100"
}
```

**Grant created via API Key:**
```json
{
    "user_id": "550e8400-e29b-41d4-a716-446655440000",
    "key_id": "880e8400-e29b-41d4-a716-446655440003",
    "key_type": "api",
    "action": "grant.created",
    "resource_type": "grant",
    "resource_id": "990e8400-e29b-41d4-a716-446655440004",
    "details": {"database": "production", "access_level": "read"},
    "ip_address": "10.0.0.50"
}
```

**Password changed via username/password:**
```json
{
    "user_id": "550e8400-e29b-41d4-a716-446655440000",
    "key_id": null,
    "key_type": "password",
    "action": "user.password_changed",
    "resource_type": "user",
    "resource_id": "550e8400-e29b-41d4-a716-446655440000",
    "details": {},
    "ip_address": "192.168.1.100"
}
```

### Why Track the Key?

1. **Accountability**: Know exactly which API key or session was used for each action
2. **Incident Response**: If a key is compromised, identify all actions taken with it
3. **Compliance**: Many security frameworks require tracking authentication method
4. **Key Rotation**: Understand key usage patterns before rotating/revoking

## Security Considerations

### Rate Limiting

All credential-handling endpoints (username/password) are rate limited:
- `POST /api/v1/auth/login` - rate limited by username
- `PUT /api/v1/auth/password` - rate limited by username
- `PUT /api/v1/users/:uid/password` - rate limited by username

Backoff delays after failures:
- 3+ failures: 5 second delay
- 5+ failures: 30 second delay
- 7+ failures: 2 minute delay
- 10+ failures: 5 minute delay

### Why Block Login Before Password Change?

1. **Security**: Temporary/initial passwords should never grant access
2. **Audit**: Clear separation between "account created" and "user activated"
3. **Compliance**: Many security frameworks require password change on first login
4. **Simplicity**: No middleware complexity checking password_changed on every request

### Why Separate Password Change Endpoint?

The `PUT /api/v1/auth/password` endpoint (without token) exists because:
1. Users can't get a token without changing their password first
2. Provides a clean bootstrap path for new users
3. Keeps credential handling isolated to specific endpoints

## Migration from Current Implementation

### Current Behavior

1. User can login with unchanged password
2. Token is created
3. `requirePasswordChanged` middleware blocks API access
4. User must change password while "logged in" but blocked

### New Behavior

1. User cannot login with unchanged password (no token created)
2. User changes password via dedicated endpoint (no token needed)
3. User logs in and gets token
4. All APIs work normally

### Migration Steps

1. Add `PUT /api/v1/auth/password` endpoint
2. Modify `POST /api/v1/auth/login` to check `password_changed`
3. Update frontend to handle `password_change_required` error
4. Remove `requirePasswordChanged` middleware (no longer needed)
5. Update E2E tests

## OpenAPI Specification Updates

```yaml
paths:
  /api/v1/auth/login:
    post:
      summary: Login and obtain session token
      description: |
        Authenticates user with username/password and returns a session token.
        Returns 403 if password change is required.
      operationId: login
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [username, password]
              properties:
                username:
                  type: string
                password:
                  type: string
                  format: password
      responses:
        '200':
          description: Login successful
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/LoginResponse'
        '401':
          description: Invalid credentials
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '403':
          description: Password change required
          content:
            application/json:
              schema:
                type: object
                properties:
                  error:
                    type: string
                    enum: [password_change_required]
                  message:
                    type: string
        '429':
          description: Rate limited
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/RateLimitError'

  /api/v1/auth/password:
    put:
      summary: Change password (pre-login)
      description: |
        Allows users to change their password before logging in.
        Used for initial password change or password reset flows.
        Does not require authentication - validates username/current_password.
      operationId: changePasswordPreLogin
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [username, current_password, new_password]
              properties:
                username:
                  type: string
                current_password:
                  type: string
                  format: password
                new_password:
                  type: string
                  format: password
      responses:
        '200':
          description: Password changed successfully
          content:
            application/json:
              schema:
                type: object
                properties:
                  message:
                    type: string
        '400':
          description: Weak password
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '401':
          description: Invalid credentials
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '429':
          description: Rate limited
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/RateLimitError'
```

## Testing Requirements

### Unit Tests

1. Login with unchanged password returns 403
2. Login with changed password returns 200 + token
3. Password change endpoint validates credentials
4. Password change sets password_changed = true
5. Rate limiting applies to both endpoints

### E2E Tests

1. New user flow: login blocked → change password → login succeeds
2. Password change with wrong current password fails
3. Weak password rejected
4. Rate limiting after multiple failures
