# Require Password Change Before API Access

## Problem

Users are created with initial passwords set by administrators. These passwords should be changed before users can perform any operations, ensuring users have taken ownership of their credentials.

## Solution

All users must change their password before accessing any API endpoint, except for the password change endpoint itself.

## Design

### Database Changes

Add a `password_changed_at` column to the `users` table:

```sql
ALTER TABLE users ADD COLUMN password_changed_at TIMESTAMPTZ;
```

- `NULL` means the password has never been changed
- Set to `NOW()` when the user updates their password

### Model Changes

Update the `User` model in `internal/store/models.go`:

```go
type User struct {
    // ... existing fields ...
    PasswordChangedAt *time.Time `bun:"password_changed_at" json:"-"`
}

// HasChangedPassword returns true if the user has changed their password
func (u *User) HasChangedPassword() bool {
    return u.PasswordChangedAt != nil
}
```

### Store Changes

Update `UpdateUser` in `internal/store/users.go` to set `password_changed_at` when password is updated:

```go
if updates.PasswordHash != nil {
    q = q.Set("password_hash = ?", *updates.PasswordHash)
    q = q.Set("password_changed_at = ?", time.Now())
}
```

### API Middleware

Add middleware in `internal/api/middleware.go`:

```go
// requirePasswordChanged blocks users who haven't changed their initial password
func (s *Server) requirePasswordChanged() gin.HandlerFunc {
    return func(c *gin.Context) {
        user := getCurrentUser(c)
        if user == nil {
            c.Next()
            return
        }

        if !user.HasChangedPassword() {
            c.JSON(http.StatusForbidden, gin.H{
                "error":   "password_change_required",
                "message": "You must change your password before accessing the API",
            })
            c.Abort()
            return
        }

        c.Next()
    }
}
```

### Route Changes

Apply middleware globally after authentication, with an exception for password change:

```go
authenticated.Use(s.authMiddleware())
authenticated.Use(s.requirePasswordChanged())

// Password change endpoint does NOT require password to have been changed
authenticated.PUT("/users/:uid/password", s.handleUpdateUserPassword)
```

Alternatively, keep `PUT /users/:uid` but skip the check when only updating password (simpler approach).

### Migration

`internal/migrations/sql/20260109100000_password_changed_at.up.sql`:

```sql
ALTER TABLE users ADD COLUMN password_changed_at TIMESTAMPTZ;
```

`internal/migrations/sql/20260109100000_password_changed_at.down.sql`:

```sql
ALTER TABLE users DROP COLUMN password_changed_at;
```

## API Response

**HTTP Status:** `403 Forbidden`

```json
{
    "error": "password_change_required",
    "message": "You must change your password before accessing the API"
}
```

The `error` field uses a machine-readable code for client handling.

## User Flow

1. Admin creates user with initial password
2. User authenticates with initial password
3. User attempts any API call -> receives `403` with `password_change_required`
4. User calls `PUT /api/users/:uid` with new password
5. `password_changed_at` is set
6. User can now access all API endpoints

## Authentication Rate Limiting

Rate limiting must be extended to authentication attempts, but only for failed attempts. This prevents brute-force attacks while ensuring legitimate users are never blocked by an attacker's activity.

### Design

Track failed authentication attempts per username (not IP), with exponential backoff:

```go
// authFailureTracker tracks failed login attempts per username
type authFailureTracker struct {
    mu       sync.RWMutex
    failures map[string]*failureRecord
}

type failureRecord struct {
    count     int
    lastFail  time.Time
    blockedUntil time.Time
}
```

### Backoff Strategy

| Failed Attempts | Delay Before Next Attempt |
|-----------------|---------------------------|
| 1-2             | None                      |
| 3-4             | 5 seconds                 |
| 5-6             | 30 seconds                |
| 7-9             | 2 minutes                 |
| 10+             | 5 minutes                 |

### Behavior

- **Failed attempt**: Increment counter, apply delay if threshold reached
- **Successful attempt**: Reset counter to zero
- Counter resets automatically after 15 minutes of no attempts

### API Response

When rate limited due to failed attempts:

**HTTP Status:** `429 Too Many Requests`

```json
{
    "error": "auth_rate_limited",
    "message": "Too many failed login attempts. Try again in 30 seconds.",
    "retry_after": 30
}
```

### Key Principle

Rate limiting is per-username, not per-IP. This ensures:
- Attackers cannot lock out legitimate users by failing from their IP
- An attacker trying to brute-force a specific account gets blocked
- Legitimate users can always authenticate if they know their password

### Implementation Notes

- Check rate limit **before** verifying credentials (fail fast)
- Only increment failure count **after** credential verification fails
- Include `Retry-After` header in 429 responses
- Log rate-limited attempts for security monitoring

## Testing

### Password Change Requirement

1. Create user via admin
2. Authenticate as new user, call `GET /api/databases` -> expect 403 with `password_change_required`
3. Call `PUT /api/users/:uid` with new password -> expect 200
4. Call `GET /api/databases` again -> expect 200

### Authentication Rate Limiting

1. Attempt login with wrong password 3 times -> all return 401
2. Attempt login with wrong password again -> expect 429 with `retry_after: 5`
3. Wait 5 seconds, attempt with correct password -> expect 200
4. Verify failure counter is reset (can fail twice again without rate limit)
