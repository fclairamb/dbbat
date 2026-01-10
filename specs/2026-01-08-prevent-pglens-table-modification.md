# Prevent PgLens Table Modification via Proxy

## Problem Statement

When a user connects through the PgLens proxy, they could potentially issue SQL queries that modify PgLens internal tables (such as `users`, `access_grants`, `databases`, etc.) if:

1. The target database is the same PostgreSQL instance that stores PgLens metadata
2. The database credentials used for the upstream connection have permissions on the pglens schema
3. A malicious or curious user crafts queries to escalate privileges

### Attack Scenarios

**Scenario 1: Password Modification**
```sql
UPDATE users SET password_hash = '$argon2id$...' WHERE username = 'attacker';
```
A user could change their own password hash to a known value, or change another user's password.

**Scenario 2: Grant Escalation**
```sql
UPDATE access_grants SET access_level = 'write', expires_at = '2099-01-01' WHERE user_id = 123;
```
A user could escalate their access level or extend their grant indefinitely.

**Scenario 3: Admin Promotion**
```sql
UPDATE users SET is_admin = true WHERE username = 'attacker';
```
A user could promote themselves to admin.

**Scenario 4: Audit Log Tampering**
```sql
DELETE FROM audit_log WHERE user_id = 123;
DELETE FROM queries WHERE user_id = 123;
```
A user could delete evidence of their activities.

## Proposed Solution

### Option A: Block Queries Targeting PgLens Tables (Recommended)

Implement query inspection to detect and block any DML/DDL operations on PgLens internal tables.

**Protected Tables:**
- `users`
- `databases`
- `access_grants`
- `connections`
- `queries`
- `query_result_rows`
- `audit_log`
- `bun_migrations`
- `bun_migration_locks`

**Implementation:**

```go
// internal/proxy/intercept.go

var pglensProtectedTables = []string{
    "users",
    "databases",
    "access_grants",
    "connections",
    "queries",
    "query_result_rows",
    "audit_log",
    "bun_migrations",
    "bun_migration_locks",
}

// isPglensTableModification checks if a query attempts to modify PgLens internal tables
func isPglensTableModification(sql string) bool {
    upper := strings.ToUpper(strings.TrimSpace(sql))

    // Check for write operations
    isWrite := false
    for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE", "ALTER"} {
        if strings.HasPrefix(upper, prefix) {
            isWrite = true
            break
        }
    }

    if !isWrite {
        return false
    }

    // Check if any protected table is referenced
    for _, table := range pglensProtectedTables {
        // Match table name with word boundaries to avoid false positives
        // e.g., "users" shouldn't match "app_users"
        pattern := `\b` + strings.ToUpper(table) + `\b`
        if matched, _ := regexp.MatchString(pattern, upper); matched {
            return true
        }
    }

    return false
}

var ErrPglensTableModification = errors.New("modification of PgLens internal tables is not permitted")
```

**Integration in Query Handler:**

```go
func (s *Session) handleQuery(query *pgproto3.Query) error {
    sqlText := query.String

    // Check quotas before executing query
    if err := s.checkQuotas(); err != nil {
        return err
    }

    // Check for read-only enforcement
    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    // NEW: Check for PgLens table modification attempts
    if isPglensTableModification(sqlText) {
        return ErrPglensTableModification
    }

    // ... rest of handler
}
```

**Pros:**
- Simple to implement
- Works regardless of database configuration
- Provides defense-in-depth

**Cons:**
- Regex-based detection could have edge cases
- Could block legitimate tables with same names in user schemas

### Option B: Schema-Qualified Detection

More precise detection that considers schema qualification:

```go
func isPglensTableModification(sql string) bool {
    upper := strings.ToUpper(strings.TrimSpace(sql))

    // Check for write operations
    if !isWriteQuery(upper) {
        return false
    }

    // Check for explicit pglens schema reference
    if strings.Contains(upper, "PGLENS.") {
        return true
    }

    // Check for public schema (default) with protected table names
    for _, table := range pglensProtectedTables {
        patterns := []string{
            `\bPUBLIC\.` + strings.ToUpper(table) + `\b`,
            `\b` + strings.ToUpper(table) + `\b`,
        }
        for _, pattern := range patterns {
            if matched, _ := regexp.MatchString(pattern, upper); matched {
                return true
            }
        }
    }

    return false
}
```

### Option C: Dedicated PgLens Schema

Use a dedicated schema (e.g., `pglens`) for all PgLens tables and block any queries referencing that schema:

```go
func isPglensSchemaAccess(sql string) bool {
    upper := strings.ToUpper(strings.TrimSpace(sql))
    return strings.Contains(upper, "PGLENS.") ||
           regexp.MustCompile(`\bSET\s+SEARCH_PATH\b.*\bPGLENS\b`).MatchString(upper)
}
```

**Pros:**
- Clear separation between PgLens and application data
- Simple detection logic

**Cons:**
- Requires migration to move tables to dedicated schema
- User could still reference tables without schema prefix if search_path is set

### Option D: Database-Level Isolation (Recommended for Production)

Configure PgLens to use a separate PostgreSQL database from any proxied databases:

**Best Practice:** The PgLens storage database should be a different database (or even a different PostgreSQL instance) from any target databases configured in PgLens.

This can be enforced through:
1. Documentation and operational guidance
2. A configuration check that warns/errors if a database configuration points to the same database as PGL_DSN
3. Using a separate PostgreSQL user for PgLens storage with no access to target databases

## Recommendation

Implement a **layered approach**:

1. **Option A (Required):** Query-level blocking as defense-in-depth
2. **Option D (Recommended):** Operational isolation guidance in documentation

## Additional Considerations

### Extended Query Protocol

The same checks must apply to Parse messages in the Extended Query Protocol:

```go
func (s *Session) handleParse(msg *pgproto3.Parse) error {
    sqlText := msg.Query

    // Check for read-only enforcement at Parse time
    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    // NEW: Check for PgLens table modification attempts
    if isPglensTableModification(sqlText) {
        return ErrPglensTableModification
    }

    // ... rest of handler
}
```

### Audit Logging

All blocked attempts should be logged to the audit log:

```go
if isPglensTableModification(sqlText) {
    s.store.LogAuditEvent(AuditEvent{
        UserID:    s.user.ID,
        Action:    "blocked_query",
        Details:   "Attempted modification of PgLens internal table",
        Query:     sqlText,
        Timestamp: time.Now(),
    })
    return ErrPglensTableModification
}
```

### Configuration Option

Allow administrators to disable this protection if needed (e.g., for maintenance):

```yaml
security:
  block_pglens_table_modification: true  # default: true
```

## Testing

```go
func TestIsPglensTableModification(t *testing.T) {
    tests := []struct {
        sql      string
        expected bool
    }{
        // Should block
        {"UPDATE users SET password_hash = 'x' WHERE id = 1", true},
        {"DELETE FROM access_grants WHERE id = 1", true},
        {"INSERT INTO users (username) VALUES ('x')", true},
        {"DROP TABLE users", true},
        {"TRUNCATE audit_log", true},
        {"ALTER TABLE databases ADD COLUMN x TEXT", true},
        {"update USERS set is_admin = true", true},  // case insensitive

        // Should allow
        {"SELECT * FROM users", false},  // reads are OK
        {"UPDATE app_users SET name = 'x'", false},  // different table
        {"INSERT INTO my_users VALUES (1)", false},  // different table
        {"UPDATE user_settings SET theme = 'dark'", false},  // different table
        {"SELECT * FROM queries", false},  // reads are OK
    }

    for _, tt := range tests {
        t.Run(tt.sql, func(t *testing.T) {
            result := isPglensTableModification(tt.sql)
            if result != tt.expected {
                t.Errorf("isPglensTableModification(%q) = %v, want %v", tt.sql, result, tt.expected)
            }
        })
    }
}
```

## Implementation Steps

1. Add `isPglensTableModification()` function to `internal/proxy/intercept.go`
2. Add `ErrPglensTableModification` error variable
3. Integrate check in `handleQuery()` and `handleParse()`
4. Add audit logging for blocked attempts
5. Add tests for the detection function
6. Update documentation with isolation recommendations
