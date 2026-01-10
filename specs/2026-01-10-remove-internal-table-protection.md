# Remove Internal Table Protection

## Summary

This specification proposes removing the planned internal table protection feature (from `2026-01-08-prevent-pglens-table-modification.md`) and instead relying on proper architectural separation: the DBBat storage database should never be configured as a target database.

## Background

The original spec proposed query-level inspection to block DML/DDL operations on DBBat internal tables (`users`, `databases`, `access_grants`, etc.) when users connect through the proxy. The rationale was defense-in-depth against scenarios where:

1. The target database is the same PostgreSQL instance as DBBat storage
2. A malicious user crafts queries to escalate privileges or tamper with audit logs

## Why Remove This Protection

### 1. Architectural Misuse Should Not Be Protected Against

The protection addresses a scenario that represents a fundamental architectural error: using the DBBat storage database as a proxy target. This is analogous to:

- Giving production database credentials to a public API
- Running a database without authentication because "it's behind a firewall"
- Storing encryption keys in the same database as encrypted data

If an administrator configures DBBat's own storage database as a target, the protection provides a false sense of security. The real solution is to not make this configuration mistake in the first place.

### 2. False Positive Risk

The proposed regex-based detection matches table names without full SQL parsing:

```go
pattern := `\b` + strings.ToUpper(table) + `\b`
```

This would block legitimate operations on user tables named:
- `users` (extremely common table name)
- `queries` (analytics applications)
- `connections` (networking applications)
- `databases` (multi-tenant applications)

The original spec even acknowledges this:

> **Cons:** Could block legitimate tables with same names in user schemas

This is not a minor edge case. `users` is one of the most common table names in existence.

### 3. SQL Parsing is Fundamentally Hard

Proper detection requires full SQL parsing to handle:
- Schema-qualified names (`myschema.users` vs `public.users`)
- Quoted identifiers (`"Users"` vs `users`)
- CTEs (`WITH users AS (...) UPDATE users ...`)
- Subqueries (`UPDATE x SET y = (SELECT ... FROM users)`)
- Dynamic SQL (`EXECUTE format('UPDATE %I SET ...', 'users')`)
- Prepared statements with computed table names
- Database links and foreign data wrappers
- PL/pgSQL with dynamic SQL

No regex or simple pattern matching can handle all these cases correctly. Building a full SQL parser for this single feature is disproportionate complexity.

### 4. Separation of Concerns Violation

The proxy layer should be agnostic to the storage layer. Embedding knowledge of storage table names into the proxy:

- Creates tight coupling between unrelated components
- Requires updating the proxy when storage schema changes
- Makes testing more complex
- Violates the single responsibility principle

### 5. Incomplete Protection

Even with query-level blocking, the protection has gaps:

- **Read access remains open**: Users could still `SELECT * FROM users` to dump password hashes (even though hashed, this is sensitive)
- **Schema manipulation**: `CREATE TABLE users (...)` in a different schema could create confusion
- **Information disclosure**: `SELECT table_name FROM information_schema.tables` reveals internal schema
- **PostgreSQL functions**: System functions could be exploited in ways not covered by table-name matching

If we accept that the storage database might be accessible, we need comprehensive protection, not just DML blocking.

### 6. Admin-Level Problem, Admin-Level Solution

Only administrators can configure target databases. If an admin configures DBBat's storage database as a target, they either:

1. Made a mistake (should be caught by configuration validation)
2. Intentionally did so (in which case, they've already compromised security)

Administrators with database configuration access already have effectively full access to the system. Protecting against their own configurations is theatrical.

## Proposed Alternative

### Configuration-Time Validation

Instead of query-time blocking, validate database configurations when they are created or updated:

```go
// internal/store/databases.go

func (s *Store) CreateDatabase(ctx context.Context, db *Database) error {
    // Check if target matches storage DSN
    if s.matchesStorageDSN(db.Host, db.Port, db.DatabaseName) {
        return ErrTargetMatchesStorage
    }
    // ... proceed with creation
}

func (s *Store) matchesStorageDSN(host string, port int, dbName string) bool {
    // Parse storage DSN and compare
    // Account for localhost vs 127.0.0.1, default ports, etc.
}
```

This approach:
- Catches the problem before it can be exploited
- Provides clear error messages to administrators
- Has zero runtime overhead on query execution
- Has no false positive risk
- Doesn't require SQL parsing

### Documentation

Add clear documentation stating:

> **Security Requirement**: The DBBat storage database must never be configured as a target database. DBBat stores sensitive information (password hashes, audit logs, access grants) that should not be accessible through the proxy. Use a separate database, or preferably a separate PostgreSQL instance, for DBBat storage.

### Startup Warning

Log a warning at startup if any configured target database appears to match the storage DSN:

```go
func (s *Server) checkDatabaseConfigurations(ctx context.Context) {
    databases, _ := s.store.ListDatabases(ctx)
    for _, db := range databases {
        if s.store.MatchesStorageDSN(db.Host, db.Port, db.DatabaseName) {
            slog.Warn("database configuration matches storage DSN",
                "database_name", db.Name,
                "target", fmt.Sprintf("%s:%d/%s", db.Host, db.Port, db.DatabaseName),
                "recommendation", "use a separate database for DBBat storage")
        }
    }
}
```

## Implementation Plan

1. **Do not implement** the query-level protection from `2026-01-08-prevent-pglens-table-modification.md`

2. **Add configuration validation** in `internal/store/databases.go`:
   - `matchesStorageDSN()` function to compare connection parameters
   - Return error from `CreateDatabase()` and `UpdateDatabase()` if target matches storage

3. **Add startup check** in `internal/api/server.go` or `cmd/dbbat/main.go`:
   - Warn if any existing database configurations match storage DSN
   - This handles databases configured before the validation was added

4. **Update documentation** in `CLAUDE.md` and `docs/`:
   - Explicit security guidance about database separation
   - Rationale for why this separation is required

5. **Archive the original spec**:
   - Mark `2026-01-08-prevent-pglens-table-modification.md` as superseded by this spec

## Comparison

| Aspect | Query-Level Blocking | Configuration Validation |
|--------|---------------------|-------------------------|
| False positives | High (common table names) | None |
| Implementation complexity | High (SQL parsing) | Low (string comparison) |
| Runtime overhead | Every query | Only on config changes |
| Maintenance burden | High (schema changes) | Low |
| Coverage | Partial (DML only) | Complete (prevents access) |
| User experience | Confusing errors | Clear admin feedback |

## Decision

Remove the internal table protection feature. Rely on:

1. Configuration-time validation (prevent the misconfiguration)
2. Documentation (educate administrators)
3. Startup warnings (catch existing misconfigurations)

The protection adds complexity and false positive risk while providing incomplete security. The root cause (misconfiguration) should be prevented directly.

## References

- Supersedes: `2026-01-08-prevent-pglens-table-modification.md`
