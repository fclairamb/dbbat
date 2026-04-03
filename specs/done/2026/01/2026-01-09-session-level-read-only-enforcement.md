# Session-Level Read-Only Enforcement

## Problem Statement

Currently, DBBat enforces read-only access by inspecting individual queries and blocking write operations (INSERT, UPDATE, DELETE, etc.). However, this approach has limitations:

1. **Incomplete Coverage:** Query inspection must maintain a list of write operations and may miss edge cases or new SQL features
2. **Performance Overhead:** Every query must be parsed and inspected before execution
3. **Maintenance Burden:** As PostgreSQL evolves, new write operations must be added to the detection logic
4. **Bypass Risk:** Complex queries or PostgreSQL features might slip through the inspection logic

### Alternative Approach: PostgreSQL Session-Level Enforcement

PostgreSQL provides a built-in session parameter `default_transaction_read_only` that, when set to `on`, prevents all write operations at the database level:

```sql
SET SESSION default_transaction_read_only = on;
```

Once set, PostgreSQL itself enforces read-only mode for all transactions in that session, regardless of the SQL syntax used. This provides:

- **Complete Coverage:** PostgreSQL handles all current and future write operations
- **Better Performance:** No proxy-level query inspection needed
- **Zero Maintenance:** No need to update write operation detection logic
- **Defense in Depth:** Works alongside query inspection as an additional layer

### Attack Scenario

If a malicious user discovers that read-only enforcement is only at the query inspection level, they might:

1. Find SQL syntax that bypasses detection
2. Disable the read-only mode if PostgreSQL allows it: `SET SESSION default_transaction_read_only = off;`
3. Execute write operations freely

## Proposed Solution

### Step 1: Set Read-Only Mode at Session Start

When a user with `access_level = 'read'` connects, send the following immediately after authenticating to the upstream database:

```go
// internal/proxy/session.go

func (s *Session) initializeUpstreamConnection() error {
    // ... existing authentication code ...

    // Enforce read-only mode at the database level if grant is read-only
    if s.grant.AccessLevel == "read" {
        if err := s.setSessionReadOnly(); err != nil {
            return fmt.Errorf("failed to set read-only mode: %w", err)
        }
    }

    return nil
}

func (s *Session) setSessionReadOnly() error {
    // Send SET SESSION command to upstream database
    query := &pgproto3.Query{
        String: "SET SESSION default_transaction_read_only = on;",
    }

    if err := s.upstream.Send(query); err != nil {
        return fmt.Errorf("send SET SESSION: %w", err)
    }

    // Read response from upstream
    for {
        msg, err := s.upstream.Receive()
        if err != nil {
            return fmt.Errorf("receive response: %w", err)
        }

        switch msg.(type) {
        case *pgproto3.CommandComplete:
            // Success - read-only mode is now enforced
            continue
        case *pgproto3.ReadyForQuery:
            // Session is ready
            return nil
        case *pgproto3.ErrorResponse:
            return fmt.Errorf("upstream error setting read-only mode: %v", msg)
        default:
            continue
        }
    }
}
```

### Step 2: Block Attempts to Disable Read-Only Mode

Even though PostgreSQL enforces the read-only mode, a user might try to disable it. We should detect and block these attempts at the proxy level:

```go
// internal/proxy/intercept.go

var readOnlyBypassPatterns = []*regexp.Regexp{
    // SET [SESSION] default_transaction_read_only (=|TO) (off|false|0)
    regexp.MustCompile(`(?i)\bSET\s+(?:SESSION\s+)?default_transaction_read_only\s*(?:=|TO)\s*(?:off|false|0)\b`),

    // RESET [SESSION] default_transaction_read_only
    regexp.MustCompile(`(?i)\bRESET\s+(?:SESSION\s+)?default_transaction_read_only\b`),

    // SET [SESSION] AUTHORIZATION (privilege escalation)
    regexp.MustCompile(`(?i)\bSET\s+(?:SESSION\s+)?AUTHORIZATION\b`),

    // SET ROLE (privilege escalation)
    regexp.MustCompile(`(?i)\bSET\s+ROLE\b`),
}

var ErrReadOnlyBypassAttempt = errors.New("attempt to disable read-only mode is not permitted")

// isReadOnlyBypassAttempt checks if a query attempts to disable read-only mode
func isReadOnlyBypassAttempt(sql string) bool {
    for _, pattern := range readOnlyBypassPatterns {
        if pattern.MatchString(sql) {
            return true
        }
    }
    return false
}
```

### Step 3: Integration in Query Handlers

Both Simple Query and Extended Query protocols must check for bypass attempts:

```go
// internal/proxy/session.go

func (s *Session) handleQuery(query *pgproto3.Query) error {
    sqlText := query.String

    // Check quotas before executing query
    if err := s.checkQuotas(); err != nil {
        return err
    }

    // NEW: Block attempts to disable read-only mode
    if s.grant.AccessLevel == "read" && isReadOnlyBypassAttempt(sqlText) {
        return ErrReadOnlyBypassAttempt
    }

    // Existing check for write queries (keep as defense-in-depth)
    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    // Check for DBBat table modification attempts
    if isPglensTableModification(sqlText) {
        return ErrPglensTableModification
    }

    // ... rest of handler
}

func (s *Session) handleParse(msg *pgproto3.Parse) error {
    sqlText := msg.Query

    // NEW: Block attempts to disable read-only mode
    if s.grant.AccessLevel == "read" && isReadOnlyBypassAttempt(sqlText) {
        return ErrReadOnlyBypassAttempt
    }

    // Existing check for write queries (keep as defense-in-depth)
    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    // Check for DBBat table modification attempts
    if isPglensTableModification(sqlText) {
        return ErrPglensTableModification
    }

    // ... rest of handler
}
```

## Benefits

### Defense in Depth

This approach provides **two layers** of read-only enforcement:

1. **Proxy-level inspection:** DBBat blocks write queries before sending them to PostgreSQL
2. **Database-level enforcement:** Even if a query bypasses proxy inspection, PostgreSQL blocks it

### Future-Proof

PostgreSQL handles all write operations, including:
- New SQL features added in future PostgreSQL versions
- Edge cases or syntax variations we might miss in regex patterns
- DDL operations (CREATE, ALTER, DROP, TRUNCATE)
- COPY FROM operations
- Function calls that modify data

### Performance

The session-level setting is applied once per connection. Most queries don't need inspection since PostgreSQL enforces the restriction.

## Trade-offs

### Legitimate Use Cases for Changing Settings

Some database tools or applications might try to change session settings as part of their normal operation. However:

- **Read-only users shouldn't need to disable read-only mode** - by definition, they only read data
- If a tool fails because it can't change this setting, it's working as intended
- The error message will clearly indicate that read-only mode cannot be disabled

### Transaction-Level Settings

Users can still set transaction-level read/write mode:
```sql
BEGIN READ WRITE;  -- Will fail due to default_transaction_read_only = on
```

This is intentional - PostgreSQL will reject this because the session default is read-only.

## Testing

### Unit Tests

```go
func TestIsReadOnlyBypassAttempt(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name     string
        sql      string
        expected bool
    }{
        // Should block
        {
            name:     "SET SESSION with off",
            sql:      "SET SESSION default_transaction_read_only = off;",
            expected: true,
        },
        {
            name:     "SET SESSION with false",
            sql:      "SET SESSION default_transaction_read_only = false;",
            expected: true,
        },
        {
            name:     "SET with TO syntax",
            sql:      "SET default_transaction_read_only TO off;",
            expected: true,
        },
        {
            name:     "RESET command",
            sql:      "RESET default_transaction_read_only;",
            expected: true,
        },
        {
            name:     "SET SESSION AUTHORIZATION",
            sql:      "SET SESSION AUTHORIZATION postgres;",
            expected: true,
        },
        {
            name:     "SET ROLE",
            sql:      "SET ROLE admin;",
            expected: true,
        },
        {
            name:     "case insensitive",
            sql:      "set session default_transaction_read_only = OFF;",
            expected: true,
        },

        // Should allow
        {
            name:     "SET to on",
            sql:      "SET SESSION default_transaction_read_only = on;",
            expected: false,
        },
        {
            name:     "SELECT query",
            sql:      "SELECT * FROM users;",
            expected: false,
        },
        {
            name:     "SET other parameter",
            sql:      "SET statement_timeout = 30000;",
            expected: false,
        },
        {
            name:     "SHOW command",
            sql:      "SHOW default_transaction_read_only;",
            expected: false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()

            result := isReadOnlyBypassAttempt(tt.sql)
            if result != tt.expected {
                t.Errorf("isReadOnlyBypassAttempt(%q) = %v, want %v", tt.sql, result, tt.expected)
            }
        })
    }
}
```

### Integration Tests

```go
func TestReadOnlySessionEnforcement(t *testing.T) {
    // Setup test database and read-only grant
    // ...

    // Test 1: Verify SET SESSION is sent at connection start
    conn, err := pgx.Connect(ctx, connString)
    require.NoError(t, err)
    defer conn.Close(ctx)

    // Query the session setting
    var readOnly bool
    err = conn.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&readOnly)
    require.NoError(t, err)
    assert.True(t, readOnly, "Session should be in read-only mode")

    // Test 2: Verify write operations are blocked by PostgreSQL
    _, err = conn.Exec(ctx, "CREATE TABLE test_table (id INT);")
    assert.Error(t, err)
    assert.Contains(t, err.Error(), "read-only transaction")

    // Test 3: Verify attempts to disable read-only mode are blocked by proxy
    _, err = conn.Exec(ctx, "SET SESSION default_transaction_read_only = off;")
    assert.Error(t, err)
    assert.Contains(t, err.Error(), "attempt to disable read-only mode is not permitted")
}
```

### Manual Testing

```bash
# Connect with read-only grant
PGPASSWORD=viewer psql -h localhost -p 5001 -U viewer -d proxy_target

# Verify session is read-only
SHOW default_transaction_read_only;
-- Expected: on

# Attempt write operation (should fail from PostgreSQL)
CREATE TABLE test (id INT);
-- Expected: ERROR:  cannot execute CREATE TABLE in a read-only transaction

# Attempt to disable read-only mode (should fail from proxy)
SET SESSION default_transaction_read_only = off;
-- Expected: ERROR:  attempt to disable read-only mode is not permitted
```

## Implementation Steps

1. ✅ **Add `setSessionReadOnly()` function** to `internal/proxy/session.go`
2. ✅ **Call `setSessionReadOnly()`** in `initializeUpstreamConnection()` when `grant.AccessLevel == "read"`
3. ✅ **Add bypass detection patterns** to `internal/proxy/intercept.go`
4. ✅ **Add `isReadOnlyBypassAttempt()` function** to `internal/proxy/intercept.go`
5. ✅ **Add `ErrReadOnlyBypassAttempt` error** variable
6. ✅ **Integrate bypass check** in `handleQuery()` and `handleParse()`
7. ✅ **Add unit tests** for `isReadOnlyBypassAttempt()`
8. ✅ **Add integration tests** for session-level enforcement
9. ✅ **Update documentation** with read-only mode behavior

## Additional Considerations

### Audit Logging

All blocked bypass attempts should be logged:

```go
if s.grant.AccessLevel == "read" && isReadOnlyBypassAttempt(sqlText) {
    s.auditLog(AuditEvent{
        Action:  "blocked_read_only_bypass",
        Details: "Attempted to disable read-only mode",
        Query:   sqlText,
    })
    return ErrReadOnlyBypassAttempt
}
```

### Error Messages

Provide clear error messages to users:

```go
var ErrReadOnlyBypassAttempt = errors.New(
    "attempt to disable read-only mode is not permitted: " +
    "your access grant is read-only and cannot be changed for this session",
)
```

### Configuration

Consider making this behavior configurable (though it should be enabled by default):

```yaml
security:
  enforce_session_read_only: true  # default: true
```

### Write Grants

For users with `access_level = 'write'`, do not set the session read-only parameter. They have full read/write access.

## Compatibility

This approach is compatible with:
- All PostgreSQL versions that support `default_transaction_read_only` (PostgreSQL 7.4+)
- All PostgreSQL client libraries (they don't need to know about this setting)
- All database tools (though some might report warnings about read-only mode)

## Security Impact

This enhancement significantly improves the security posture:

1. **Stronger enforcement:** Two layers instead of one
2. **Reduced bypass risk:** PostgreSQL validates all operations
3. **Clear audit trail:** Both proxy and database log write attempts
4. **Future-proof:** Automatically covers new PostgreSQL features
