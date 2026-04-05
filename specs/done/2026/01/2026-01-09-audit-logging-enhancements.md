# Audit Logging Enhancements

## Overview

Enhance the audit logging system to provide comprehensive security event tracking suitable for compliance frameworks (ISO 27001, SOC 2, PCI-DSS). This includes logging all authentication events, API operations, and providing tamper-evident log integrity.

## Motivation

Current audit logging covers:
- User CRUD operations
- Database configuration changes
- Grant creation/revocation
- API key operations

Missing capabilities that auditors typically require:
- **Authentication events**: Failed login attempts, successful logins, session terminations
- **Comprehensive API tracking**: All read operations, not just mutations
- **Proxy events**: Connection attempts, query execution summaries
- **Log integrity**: Tamper-evident logs with hash chains
- **Export capabilities**: SIEM integration, compliance report generation

## Design

### Event Categories

#### 1. Authentication Events

| Event Type | Trigger | Details |
|------------|---------|---------|
| `auth.login.success` | Successful API Basic Auth | user_id, source_ip, user_agent |
| `auth.login.failed` | Failed API Basic Auth | username (attempted), source_ip, user_agent, reason |
| `auth.api_key.success` | Successful API key auth | user_id, key_prefix, source_ip |
| `auth.api_key.failed` | Failed API key auth | key_prefix (attempted), source_ip, reason |
| `auth.proxy.success` | Successful proxy auth | user_id, database_id, source_ip |
| `auth.proxy.failed` | Failed proxy auth | username (attempted), database (attempted), source_ip, reason |
| `auth.logout` | Explicit logout or session end | user_id, source_ip, session_duration |

**Failure reasons:**
- `invalid_credentials` - Wrong password/key
- `user_not_found` - Username doesn't exist
- `user_disabled` - Account is disabled
- `key_expired` - API key has expired
- `key_revoked` - API key was revoked
- `no_grant` - No valid grant for database
- `grant_expired` - Grant has expired
- `quota_exceeded` - Query or byte quota exceeded

#### 2. Administrative Events (existing, enhanced)

| Event Type | Trigger | Details |
|------------|---------|---------|
| `user.created` | POST /api/users | user_id, username, roles |
| `user.updated` | PATCH /api/users/:id | user_id, changed_fields |
| `user.deleted` | DELETE /api/users/:id | user_id, username |
| `user.password_changed` | Password update | user_id, changed_by_self |
| `database.created` | POST /api/databases | database_id, name |
| `database.updated` | PUT /api/databases/:id | database_id, changed_fields |
| `database.deleted` | DELETE /api/databases/:id | database_id, name |
| `grant.created` | POST /api/grants | grant_id, user_id, database_id, access_level, expires_at |
| `grant.revoked` | DELETE /api/grants/:id | grant_id, user_id, database_id, revoked_by |
| `api_key.created` | POST /api/keys | key_id, key_prefix, user_id |
| `api_key.revoked` | DELETE /api/keys/:id | key_id, key_prefix, revoked_by |

#### 3. Data Access Events (new)

| Event Type | Trigger | Details |
|------------|---------|---------|
| `data.users.list` | GET /api/users | performed_by, count |
| `data.users.view` | GET /api/users/:id | performed_by, user_id |
| `data.databases.list` | GET /api/databases | performed_by, count |
| `data.databases.view` | GET /api/databases/:id | performed_by, database_id |
| `data.grants.list` | GET /api/grants | performed_by, filters, count |
| `data.connections.list` | GET /api/connections | performed_by, filters, count |
| `data.queries.list` | GET /api/queries | performed_by, filters, count |
| `data.queries.view` | GET /api/queries/:id | performed_by, query_id |
| `data.audit.list` | GET /api/audit | performed_by, filters, count |
| `data.audit.export` | GET /api/audit/export | performed_by, format, date_range |

#### 4. Proxy Events (new)

| Event Type | Trigger | Details |
|------------|---------|---------|
| `proxy.connection.opened` | Proxy connection established | connection_id, user_id, database_id, source_ip |
| `proxy.connection.closed` | Proxy connection terminated | connection_id, duration, query_count, bytes_transferred |
| `proxy.query.executed` | Query completed | connection_id, query_id, duration_ms, rows_affected |
| `proxy.query.blocked` | Query rejected (read-only) | connection_id, sql_preview, reason |

#### 5. System Events (new)

| Event Type | Trigger | Details |
|------------|---------|---------|
| `system.startup` | Server starts | version, config_summary |
| `system.shutdown` | Graceful shutdown | reason, uptime |
| `system.config.changed` | Runtime config change | changed_keys, changed_by |
| `system.migration.applied` | Migration runs | migration_name, direction |

### Schema Enhancements

#### Updated audit_log Table

```sql
ALTER TABLE audit_log ADD COLUMN source_ip INET;
ALTER TABLE audit_log ADD COLUMN user_agent TEXT;
ALTER TABLE audit_log ADD COLUMN request_id UUID;
ALTER TABLE audit_log ADD COLUMN session_id UUID;
ALTER TABLE audit_log ADD COLUMN hash VARCHAR(64);  -- SHA-256 of previous record + current record
ALTER TABLE audit_log ADD COLUMN sequence BIGSERIAL;  -- Monotonic sequence for ordering

CREATE INDEX idx_audit_log_source_ip ON audit_log(source_ip);
CREATE INDEX idx_audit_log_request_id ON audit_log(request_id);
CREATE INDEX idx_audit_log_sequence ON audit_log(sequence);
```

#### Hash Chain Implementation

Each audit record includes a SHA-256 hash computed from:
1. Previous record's hash (or genesis hash for first record)
2. Current record's data (event_type, user_id, performed_by, details, created_at, sequence)

```go
func computeAuditHash(prev string, event *AuditLog) string {
    data := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d",
        prev,
        event.EventType,
        event.UserID,
        event.PerformedBy,
        event.Details,
        event.CreatedAt.Format(time.RFC3339Nano),
        event.Sequence,
    )
    hash := sha256.Sum256([]byte(data))
    return hex.EncodeToString(hash[:])
}
```

**Verification:** Admins can verify log integrity by recomputing the hash chain.

### Configuration

Add to koanf configuration:

```yaml
audit:
  # Event categories to log (all enabled by default)
  log_auth_success: true        # Log successful authentications
  log_auth_failed: true         # Log failed authentications
  log_data_access: true         # Log read operations (GET requests)
  log_proxy_queries: true       # Log individual query executions
  log_proxy_connections: true   # Log connection open/close

  # Hash chain for tamper evidence
  enable_hash_chain: true

  # Retention
  retention_days: 365           # Default 1 year for compliance

  # Export
  export_enabled: true
  export_formats: ["json", "csv"]
```

Environment variables:
- `DBB_AUDIT_LOG_AUTH_SUCCESS` (default: `true`)
- `DBB_AUDIT_LOG_AUTH_FAILED` (default: `true`)
- `DBB_AUDIT_LOG_DATA_ACCESS` (default: `true`)
- `DBB_AUDIT_LOG_PROXY_QUERIES` (default: `true`)
- `DBB_AUDIT_LOG_PROXY_CONNECTIONS` (default: `true`)
- `DBB_AUDIT_ENABLE_HASH_CHAIN` (default: `true`)
- `DBB_AUDIT_RETENTION_DAYS` (default: `365`)

### API Endpoints

#### Enhanced List Endpoint

```
GET /api/audit
```

New query parameters:
- `category` - Filter by category: `auth`, `admin`, `data`, `proxy`, `system`
- `source_ip` - Filter by source IP (supports CIDR notation)
- `request_id` - Filter by request ID
- `session_id` - Filter by session ID

#### Log Export Endpoint (new)

```
GET /api/audit/export
Authorization: Basic <admin:password>

Query parameters:
  format: json | csv | jsonl (default: json)
  start_time: ISO 8601 timestamp
  end_time: ISO 8601 timestamp
  category: auth | admin | data | proxy | system (optional)

Response:
  Content-Type: application/json (or text/csv)
  Content-Disposition: attachment; filename="audit-export-2026-01-09.json"
```

For large exports, returns a streaming response.

#### Hash Chain Verification Endpoint (new)

```
GET /api/audit/verify
Authorization: Basic <admin:password>

Query parameters:
  start_sequence: Starting sequence number (default: 1)
  end_sequence: Ending sequence number (default: latest)

Response (200 OK):
{
  "verified": true,
  "records_checked": 15000,
  "start_sequence": 1,
  "end_sequence": 15000,
  "first_hash": "a1b2c3...",
  "last_hash": "d4e5f6..."
}

Response (409 Conflict - integrity violation):
{
  "verified": false,
  "records_checked": 8500,
  "first_invalid_sequence": 8501,
  "expected_hash": "abc123...",
  "actual_hash": "def456...",
  "error": "Hash chain broken at sequence 8501"
}
```

### Middleware Implementation

#### Request Context Enrichment

```go
func AuditContextMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        // Generate request ID if not present
        requestID := c.GetHeader("X-Request-ID")
        if requestID == "" {
            requestID = uuid.NewString()
        }
        c.Set("request_id", requestID)
        c.Header("X-Request-ID", requestID)

        // Extract source info
        c.Set("source_ip", c.ClientIP())
        c.Set("user_agent", c.GetHeader("User-Agent"))

        c.Next()
    }
}
```

#### Auth Event Logger

Wraps the auth middleware to log authentication events:

```go
func AuthWithAudit(store *store.Store, cfg *config.Config) gin.HandlerFunc {
    return func(c *gin.Context) {
        // Attempt authentication
        user, authType, err := authenticate(c, store)

        if err != nil {
            // Log failed auth
            if cfg.Audit.LogAuthFailed {
                logAuthEvent(c, store, "failed", authType, err)
            }
            c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
            return
        }

        // Log successful auth
        if cfg.Audit.LogAuthSuccess {
            logAuthEvent(c, store, "success", authType, nil)
        }

        c.Set("user", user)
        c.Next()
    }
}
```

### Proxy Integration

Add audit hooks to the proxy session handler:

```go
func (s *Session) logProxyEvent(eventType string, details map[string]interface{}) {
    if !s.config.Audit.LogProxyConnections && strings.HasPrefix(eventType, "proxy.connection") {
        return
    }
    if !s.config.Audit.LogProxyQueries && strings.HasPrefix(eventType, "proxy.query") {
        return
    }

    s.store.LogAuditEvent(context.Background(), &store.AuditEvent{
        EventType:   eventType,
        UserID:      &s.userID,
        PerformedBy: &s.userID,
        Details:     toJSON(details),
        SourceIP:    s.sourceIP,
        SessionID:   &s.connectionUID,
    })
}
```

## Migration

Add to `internal/migrations/sql/`:

**YYYYMMDDHHMMSS_audit_logging_enhancements.up.sql**:
```sql
-- Add new columns to audit_log
ALTER TABLE audit_log ADD COLUMN source_ip INET;
ALTER TABLE audit_log ADD COLUMN user_agent TEXT;
ALTER TABLE audit_log ADD COLUMN request_id UUID;
ALTER TABLE audit_log ADD COLUMN session_id UUID;
ALTER TABLE audit_log ADD COLUMN hash VARCHAR(64);
ALTER TABLE audit_log ADD COLUMN sequence BIGSERIAL;

--bun:split

-- Create indexes for new columns
CREATE INDEX idx_audit_log_source_ip ON audit_log(source_ip);
CREATE INDEX idx_audit_log_request_id ON audit_log(request_id);
CREATE INDEX idx_audit_log_sequence ON audit_log(sequence);

--bun:split

-- Backfill sequence for existing records (maintaining created_at order)
WITH ordered AS (
    SELECT uid, ROW_NUMBER() OVER (ORDER BY created_at, uid) AS seq
    FROM audit_log
)
UPDATE audit_log SET sequence = ordered.seq
FROM ordered WHERE audit_log.uid = ordered.uid;
```

**YYYYMMDDHHMMSS_audit_logging_enhancements.down.sql**:
```sql
DROP INDEX IF EXISTS idx_audit_log_sequence;
DROP INDEX IF EXISTS idx_audit_log_request_id;
DROP INDEX IF EXISTS idx_audit_log_source_ip;

--bun:split

ALTER TABLE audit_log DROP COLUMN IF EXISTS sequence;
ALTER TABLE audit_log DROP COLUMN IF EXISTS hash;
ALTER TABLE audit_log DROP COLUMN IF EXISTS session_id;
ALTER TABLE audit_log DROP COLUMN IF EXISTS request_id;
ALTER TABLE audit_log DROP COLUMN IF EXISTS user_agent;
ALTER TABLE audit_log DROP COLUMN IF EXISTS source_ip;
```

## Implementation Plan

### Phase 1: Schema and Core Infrastructure

1. Add migration for new audit_log columns
2. Update `AuditLog` model in `internal/store/models.go`
3. Add configuration options to koanf config
4. Implement hash chain computation in `internal/store/audit.go`
5. Add `AuditContextMiddleware` for request ID and source IP tracking

### Phase 2: Authentication Events

1. Create `internal/api/audit_middleware.go`:
   - Wrap existing auth middleware with event logging
   - Log success/failure events with appropriate details
2. Update proxy auth in `internal/proxy/auth.go`:
   - Log proxy authentication success/failure
3. Add failure reason enumeration

### Phase 3: Data Access Events

1. Add audit logging to read endpoints:
   - GET /api/users, /api/users/:id
   - GET /api/databases, /api/databases/:id
   - GET /api/grants
   - GET /api/connections
   - GET /api/queries, /api/queries/:id
   - GET /api/audit
2. Make data access logging configurable (can be disabled for high-traffic deployments)

### Phase 4: Proxy Events

1. Add connection open/close logging to `internal/proxy/session.go`
2. Add query execution logging to `internal/proxy/intercept.go`
3. Add blocked query logging for read-only grant violations

### Phase 5: Export and Verification

1. Implement `GET /api/audit/export`:
   - JSON, CSV, JSONL formats
   - Streaming response for large exports
2. Implement `GET /api/audit/verify`:
   - Hash chain verification
   - Partial verification (sequence ranges)
3. Update OpenAPI spec

### Phase 6: System Events

1. Add startup/shutdown logging in `cmd/dbbat/main.go`
2. Add migration event logging
3. Add config change logging (if runtime config changes are implemented)

## Security Considerations

1. **Sensitive data exclusion**: Never log passwords, API keys, or query result data in audit logs. Only log metadata.

2. **Hash chain integrity**: The hash chain provides tamper evidence but not tamper prevention. An attacker with database access could theoretically rebuild the chain. For stronger guarantees, consider external log shipping to a WORM (Write Once Read Many) storage.

3. **Rate limiting**: High-volume deployments may need to sample or aggregate certain event types (e.g., proxy.query.executed) to avoid overwhelming the audit log.

4. **Log access**: Audit logs contain sensitive metadata (who accessed what, when). Ensure only users with viewer or admin roles can access the audit API.

5. **Retention**: Default 365-day retention balances compliance requirements with storage costs. Organizations may need longer retention for specific regulations.

## Compliance Mapping

| Requirement | Coverage |
|-------------|----------|
| **ISO 27001 A.12.4** - Logging and monitoring | Auth events, admin events, proxy events |
| **SOC 2 CC6.1** - Logical access controls | Auth success/failure, grant events |
| **SOC 2 CC7.2** - System monitoring | All event categories, hash verification |
| **PCI-DSS 10.2** - Audit trail | Auth events, data access events |
| **PCI-DSS 10.5** - Secure audit trails | Hash chain tamper evidence |
| **GDPR Art. 30** - Records of processing | Data access events, export capability |

## Testing

1. **Unit tests**:
   - Hash chain computation
   - Event detail sanitization (no sensitive data)
   - Configuration flag behavior

2. **Integration tests** (testcontainers):
   - Auth event logging (success/failure)
   - Data access event logging
   - Proxy event logging
   - Export endpoint (all formats)
   - Hash chain verification

3. **Manual testing**:
   - Verify events appear for all operations
   - Test export with large datasets
   - Verify hash chain integrity after various operations
