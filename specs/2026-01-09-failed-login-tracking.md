# Failed Login Tracking in Audit Logs

## Overview

Track all failed login attempts in the audit log system to enable security monitoring, intrusion detection, and compliance reporting. This covers both REST API authentication failures and PostgreSQL proxy connection failures.

## Motivation

Security best practices and compliance frameworks require comprehensive tracking of failed authentication attempts:

1. **Security monitoring**: Detect brute force attacks, credential stuffing, and unauthorized access attempts
2. **Compliance**: SOC 2, ISO 27001, and PCI-DSS require logging of failed authentication events
3. **Forensics**: Investigate security incidents and determine attack patterns
4. **User support**: Help users diagnose connection issues by showing detailed failure reasons

Currently, failed logins are not logged, making it impossible to:
- Detect ongoing attacks in real-time
- Generate security reports for auditors
- Alert on suspicious activity patterns
- Help users troubleshoot connection problems

## Design

### Event Types

#### REST API Failed Logins

| Event Type | Trigger | Details |
|------------|---------|---------|
| `auth.rest.login.failed` | POST /api/v1/auth/login with invalid credentials | username, source_ip, user_agent, reason, request_id |
| `auth.rest.token.invalid` | API request with invalid Bearer token | token_prefix, source_ip, user_agent, reason, request_id |
| `auth.rest.token.expired` | API request with expired token | token_prefix, user_id, source_ip, user_agent, request_id |

#### PostgreSQL Proxy Failed Logins

| Event Type | Trigger | Details |
|------------|---------|---------|
| `auth.proxy.login.failed` | PostgreSQL connection attempt with invalid credentials | username, database_name, source_ip, reason, protocol_version |
| `auth.proxy.grant.denied` | Valid user but no grant for database | user_id, database_name, source_ip, reason |
| `auth.proxy.quota.exceeded` | Valid user but quota exceeded | user_id, grant_id, database_name, source_ip, quota_type |

### Failure Reasons

Enumerate specific failure reasons for better security analysis:

#### REST API Failure Reasons
```go
const (
    // Credential failures
    ReasonInvalidUsername    = "invalid_username"      // Username not found
    ReasonInvalidPassword    = "invalid_password"      // Wrong password
    ReasonPasswordChangeReq  = "password_change_required" // Initial password not changed

    // Token failures
    ReasonTokenInvalid       = "token_invalid"         // Malformed or unknown token
    ReasonTokenExpired       = "token_expired"         // Token past expiration
    ReasonTokenRevoked       = "token_revoked"         // Token was revoked

    // Account status
    ReasonUserDisabled       = "user_disabled"         // Account disabled by admin
    ReasonUserDeleted        = "user_deleted"          // Account was deleted
)
```

#### Proxy Failure Reasons
```go
const (
    // Authentication failures
    ReasonProxyInvalidUsername = "invalid_username"    // Username not found
    ReasonProxyInvalidPassword = "invalid_password"    // Wrong password
    ReasonProxyUserDisabled    = "user_disabled"       // Account disabled

    // Authorization failures
    ReasonNoGrant              = "no_grant"            // No grant for database
    ReasonGrantExpired         = "grant_expired"       // Grant expired
    ReasonGrantNotStarted      = "grant_not_started"   // Grant not yet active
    ReasonWrongAccessLevel     = "wrong_access_level"  // Write attempt with read-only grant

    // Quota failures
    ReasonQueryQuotaExceeded   = "query_quota_exceeded"   // Max queries reached
    ReasonBytesQuotaExceeded   = "bytes_quota_exceeded"   // Max bytes reached

    // Database failures
    ReasonDatabaseNotFound     = "database_not_found"     // Database config doesn't exist
    ReasonDatabaseDisabled     = "database_disabled"      // Database disabled by admin
    ReasonUpstreamConnFailed   = "upstream_conn_failed"   // Can't connect to target database
)
```

### Audit Log Details JSON Schema

#### REST API Login Failure
```json
{
  "username": "attempted_username",
  "source_ip": "192.168.1.100",
  "user_agent": "Mozilla/5.0...",
  "reason": "invalid_password",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-01-09T10:30:45Z"
}
```

#### REST API Token Failure
```json
{
  "token_prefix": "dbb_12ab",
  "user_id": "01932e4f-3b2a-7890-8f3e-8c9f1a2b3c4d",
  "source_ip": "192.168.1.100",
  "user_agent": "curl/7.81.0",
  "reason": "token_expired",
  "request_id": "550e8400-e29b-41d4-a716-446655440001",
  "timestamp": "2026-01-09T10:31:12Z"
}
```

#### Proxy Login Failure
```json
{
  "username": "attempted_username",
  "database_name": "proxy_target",
  "source_ip": "10.0.1.50",
  "reason": "invalid_password",
  "protocol_version": "3.0",
  "timestamp": "2026-01-09T10:32:05Z"
}
```

#### Proxy Grant Denied
```json
{
  "user_id": "01932e4f-3b2a-7890-8f3e-8c9f1a2b3c4d",
  "username": "connector",
  "database_name": "proxy_target",
  "database_id": "01932e4f-4c3d-7890-9f4e-9d0e2b3c4d5e",
  "source_ip": "10.0.1.50",
  "reason": "grant_expired",
  "grant_id": "01932e4f-5d4e-7890-af5e-ad1f3c4d5e6f",
  "expired_at": "2026-01-08T00:00:00Z",
  "timestamp": "2026-01-09T10:33:20Z"
}
```

#### Proxy Quota Exceeded
```json
{
  "user_id": "01932e4f-3b2a-7890-8f3e-8c9f1a2b3c4d",
  "username": "connector",
  "grant_id": "01932e4f-5d4e-7890-af5e-ad1f3c4d5e6f",
  "database_name": "proxy_target",
  "database_id": "01932e4f-4c3d-7890-9f4e-9d0e2b3c4d5e",
  "source_ip": "10.0.1.50",
  "reason": "query_quota_exceeded",
  "quota_type": "queries",
  "quota_limit": 1000,
  "quota_used": 1000,
  "timestamp": "2026-01-09T10:34:45Z"
}
```

## Implementation

### 1. REST API Middleware Changes

Update `internal/api/middleware.go` to log failed authentication:

```go
func AuthMiddleware(store *store.Store) gin.HandlerFunc {
    return func(c *gin.Context) {
        user, authType, err := authenticate(c, store)

        if err != nil {
            // Determine failure reason
            reason := determineAuthFailureReason(err, authType)

            // Log failed authentication
            details := map[string]interface{}{
                "source_ip":  c.ClientIP(),
                "user_agent": c.GetHeader("User-Agent"),
                "reason":     reason,
                "request_id": c.GetString("request_id"),
                "timestamp":  time.Now().Format(time.RFC3339),
            }

            // Add username or token prefix depending on auth type
            switch authType {
            case "basic":
                if username := extractUsername(c); username != "" {
                    details["username"] = username
                }
            case "bearer":
                if tokenPrefix := extractTokenPrefix(c); tokenPrefix != "" {
                    details["token_prefix"] = tokenPrefix
                }
            }

            // Log to audit
            _ = store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
                EventType:   fmt.Sprintf("auth.rest.%s.failed", authType),
                Details:     toJSON(details),
            })

            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
            return
        }

        c.Set("user", user)
        c.Next()
    }
}
```

### 2. Login Endpoint Changes

Update `internal/api/auth.go` login handler to log failed attempts:

```go
func (s *APIServer) handleLogin(c *gin.Context) {
    var req LoginRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
        return
    }

    user, err := s.store.GetUserByUsername(c.Request.Context(), req.Username)
    if err != nil {
        // Log failed login - invalid username
        s.logFailedLogin(c, req.Username, ReasonInvalidUsername)
        c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
        return
    }

    // Check if password change is required
    if user.RequirePasswordChange {
        // Log failed login - password change required
        s.logFailedLogin(c, req.Username, ReasonPasswordChangeReq)
        c.JSON(http.StatusForbidden, gin.H{"error": "password_change_required"})
        return
    }

    // Verify password
    if err := crypto.VerifyPassword(user.PasswordHash, req.Password); err != nil {
        // Log failed login - invalid password
        s.logFailedLogin(c, req.Username, ReasonInvalidPassword)
        c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
        return
    }

    // Success - create session token
    // (no audit log here, as successful logins are handled separately)
    // ...
}

func (s *APIServer) logFailedLogin(c *gin.Context, username, reason string) {
    details := map[string]interface{}{
        "username":   username,
        "source_ip":  c.ClientIP(),
        "user_agent": c.GetHeader("User-Agent"),
        "reason":     reason,
        "request_id": c.GetString("request_id"),
        "timestamp":  time.Now().Format(time.RFC3339),
    }

    _ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
        EventType: "auth.rest.login.failed",
        Details:   toJSON(details),
    })
}
```

### 3. Proxy Authentication Changes

Update `internal/proxy/auth.go` to log failed proxy authentication:

```go
func (s *Session) authenticateUser(username, password, database string) error {
    // Get user
    user, err := s.store.GetUserByUsername(s.ctx, username)
    if err != nil {
        s.logProxyAuthFailure(username, database, ReasonProxyInvalidUsername, nil)
        return fmt.Errorf("authentication failed")
    }

    // Verify password
    if err := crypto.VerifyPassword(user.PasswordHash, password); err != nil {
        s.logProxyAuthFailure(username, database, ReasonProxyInvalidPassword, &user.UID)
        return fmt.Errorf("authentication failed")
    }

    // Check if user is disabled
    // (assuming we add a disabled field to users table)

    // Get database config
    dbConfig, err := s.store.GetDatabaseByName(s.ctx, database)
    if err != nil {
        s.logProxyAuthFailure(username, database, ReasonDatabaseNotFound, &user.UID)
        return fmt.Errorf("database not found")
    }

    // Check for valid grant
    grant, err := s.store.GetActiveGrant(s.ctx, user.UID, dbConfig.UID)
    if err != nil {
        reason := ReasonNoGrant
        if errors.Is(err, ErrGrantExpired) {
            reason = ReasonGrantExpired
        } else if errors.Is(err, ErrGrantNotStarted) {
            reason = ReasonGrantNotStarted
        }

        s.logProxyGrantDenied(user.UID, username, database, dbConfig.UID, reason, nil)
        return fmt.Errorf("access denied")
    }

    // Check quotas
    if grant.MaxQueries != nil {
        usage, _ := s.store.GetGrantQueryUsage(s.ctx, grant.UID)
        if usage >= *grant.MaxQueries {
            s.logProxyQuotaExceeded(user.UID, username, grant.UID, database, dbConfig.UID,
                "queries", *grant.MaxQueries, usage)
            return fmt.Errorf("query quota exceeded")
        }
    }

    if grant.MaxBytes != nil {
        usage, _ := s.store.GetGrantBytesUsage(s.ctx, grant.UID)
        if usage >= *grant.MaxBytes {
            s.logProxyQuotaExceeded(user.UID, username, grant.UID, database, dbConfig.UID,
                "bytes", *grant.MaxBytes, usage)
            return fmt.Errorf("bytes quota exceeded")
        }
    }

    s.user = user
    s.dbConfig = dbConfig
    s.grant = grant
    return nil
}

func (s *Session) logProxyAuthFailure(username, database, reason string, userID *string) {
    details := map[string]interface{}{
        "username":         username,
        "database_name":    database,
        "source_ip":        s.remoteAddr,
        "reason":           reason,
        "protocol_version": "3.0",
        "timestamp":        time.Now().Format(time.RFC3339),
    }

    event := &store.AuditEvent{
        EventType: "auth.proxy.login.failed",
        Details:   toJSON(details),
    }

    if userID != nil {
        event.UserID = userID
    }

    _ = s.store.LogAuditEvent(s.ctx, event)
}

func (s *Session) logProxyGrantDenied(userID, username, database, databaseID, reason string, grantID *string) {
    details := map[string]interface{}{
        "user_id":       userID,
        "username":      username,
        "database_name": database,
        "database_id":   databaseID,
        "source_ip":     s.remoteAddr,
        "reason":        reason,
        "timestamp":     time.Now().Format(time.RFC3339),
    }

    if grantID != nil {
        details["grant_id"] = *grantID
    }

    _ = s.store.LogAuditEvent(s.ctx, &store.AuditEvent{
        EventType:   "auth.proxy.grant.denied",
        UserID:      &userID,
        PerformedBy: &userID,
        Details:     toJSON(details),
    })
}

func (s *Session) logProxyQuotaExceeded(userID, username, grantID, database, databaseID, quotaType string, limit, used int64) {
    details := map[string]interface{}{
        "user_id":       userID,
        "username":      username,
        "grant_id":      grantID,
        "database_name": database,
        "database_id":   databaseID,
        "source_ip":     s.remoteAddr,
        "reason":        fmt.Sprintf("%s_quota_exceeded", quotaType),
        "quota_type":    quotaType,
        "quota_limit":   limit,
        "quota_used":    used,
        "timestamp":     time.Now().Format(time.RFC3339),
    }

    _ = s.store.LogAuditEvent(s.ctx, &store.AuditEvent{
        EventType:   "auth.proxy.quota.exceeded",
        UserID:      &userID,
        PerformedBy: &userID,
        Details:     toJSON(details),
    })
}
```

## Security Considerations

1. **Rate limiting**: Failed login tracking enables rate limiting and account lockout policies. Consider implementing:
   - Temporary account lockout after N failed attempts
   - Progressive delay between attempts
   - CAPTCHA challenges after repeated failures

2. **Sensitive data**: Never log passwords or full tokens in audit events. Only log:
   - Attempted usernames (no passwords)
   - Token prefixes (first 8 chars, e.g., `dbb_12ab`)
   - Failure reasons (enumerated values)

3. **Log flooding**: Attackers may attempt to flood audit logs with failed login attempts. Consider:
   - Aggregating repeated failures from same IP
   - Rate limiting audit log writes
   - Alerting on abnormal failure rates

4. **Privacy**: Failed login events may contain sensitive information (usernames, IP addresses). Ensure:
   - Only admin/viewer roles can access audit logs
   - Comply with data retention policies
   - Consider anonymization for long-term storage

## API Endpoints

Failed login events are queryable via the existing audit API:

```
GET /api/v1/audit?event_type=auth.rest.login.failed
GET /api/v1/audit?event_type=auth.proxy.login.failed
GET /api/v1/audit?event_type=auth.proxy.grant.denied
GET /api/v1/audit?event_type=auth.proxy.quota.exceeded
```

Additional filters:
- `source_ip` - Filter by source IP address
- `start_time` / `end_time` - Time range
- `user_id` - Filter by user (for grant denied / quota exceeded)

## Metrics and Alerting

Consider exposing Prometheus metrics for real-time monitoring:

```
dbbat_auth_failures_total{type="rest",reason="invalid_password"} 42
dbbat_auth_failures_total{type="rest",reason="invalid_username"} 15
dbbat_auth_failures_total{type="proxy",reason="no_grant"} 8
dbbat_auth_failures_total{type="proxy",reason="quota_exceeded"} 3
```

Alert examples:
- **Brute force detection**: Alert if >10 failed logins from same IP in 5 minutes
- **Credential stuffing**: Alert if >100 unique usernames attempted from same IP
- **Quota exhaustion**: Alert if multiple users hitting quota limits (may indicate attack or misconfiguration)

## Testing

### Unit Tests

```go
func TestLogFailedRestLogin(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name     string
        username string
        password string
        wantReason string
    }{
        {
            name:     "invalid username",
            username: "nonexistent",
            password: "anypassword",
            wantReason: ReasonInvalidUsername,
        },
        {
            name:     "invalid password",
            username: "admin",
            password: "wrongpassword",
            wantReason: ReasonInvalidPassword,
        },
        {
            name:     "password change required",
            username: "newuser",
            password: "correctpassword",
            wantReason: ReasonPasswordChangeReq,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test implementation
            // 1. Make login request
            // 2. Verify 401/403 response
            // 3. Query audit log
            // 4. Assert event_type and reason match expected
        })
    }
}

func TestLogFailedProxyLogin(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name       string
        username   string
        password   string
        database   string
        wantReason string
    }{
        {
            name:       "invalid username",
            username:   "nonexistent",
            password:   "anypassword",
            database:   "proxy_target",
            wantReason: ReasonProxyInvalidUsername,
        },
        {
            name:       "invalid password",
            username:   "connector",
            password:   "wrongpassword",
            database:   "proxy_target",
            wantReason: ReasonProxyInvalidPassword,
        },
        {
            name:       "no grant",
            username:   "viewer",
            password:   "viewer",
            database:   "unauthorized_db",
            wantReason: ReasonNoGrant,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test implementation
            // 1. Attempt proxy connection
            // 2. Verify connection rejected
            // 3. Query audit log
            // 4. Assert event_type and reason match expected
        })
    }
}
```

### E2E Tests (Playwright)

```typescript
test('failed REST login is logged', async ({ page }) => {
  // Attempt login with wrong credentials
  await page.goto('/app/login');
  await page.fill('input[name="username"]', 'admin');
  await page.fill('input[name="password"]', 'wrongpassword');
  await page.click('button[type="submit"]');

  // Should see error message
  await expect(page.locator('.error-message')).toContainText('Invalid credentials');

  // Login with correct credentials
  await page.fill('input[name="password"]', 'admintest');
  await page.click('button[type="submit"]');

  // Navigate to audit logs
  await page.goto('/app/audit');

  // Should see failed login event
  await expect(page.locator('text=auth.rest.login.failed')).toBeVisible();
  await expect(page.locator('text=invalid_password')).toBeVisible();
});
```

## Migration

No database schema changes required. Failed login tracking uses the existing `audit_log` table.

If implementing the comprehensive audit enhancements (source_ip, user_agent columns), those migrations should be applied first.

## References

- Related spec: `specs/2026-01-09-audit-logging-enhancements.md` (comprehensive audit system)
- Compliance frameworks: SOC 2 CC6.1, ISO 27001 A.12.4, PCI-DSS 10.2.4-10.2.5
