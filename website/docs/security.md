# Security

DBBat implements multiple security layers to protect both the proxy infrastructure and the target databases.

## Authentication

### Password Hashing

User passwords are hashed using **Argon2id**, the winner of the Password Hashing Competition:

- Memory-hard algorithm resistant to GPU/ASIC attacks
- Configurable memory, time, and parallelism parameters
- Includes salt to prevent rainbow table attacks

### Password Requirements

- **Mandatory change**: Users must change their initial password before accessing the API
- **Minimum length**: 8 characters (configurable)
- Login attempts before password change return `403 password_change_required`

### Authentication Rate Limiting

Failed login attempts trigger exponential backoff per username:

| Failed Attempts | Lockout Duration |
|-----------------|------------------|
| 1-2 | None |
| 3-4 | 5 seconds |
| 5-6 | 30 seconds |
| 7-9 | 2 minutes |
| 10+ | 5 minutes |

This prevents brute-force attacks while allowing legitimate users to recover from typos.

### Token Types

| Type | Prefix | Lifetime | Use Case |
|------|--------|----------|----------|
| Web Session | `web_` | 1 hour | Interactive frontend use |
| API Key | `dbb_` | Configurable (or permanent) | Programmatic access |

### API Key Restrictions

API keys have intentional limitations:

- **Cannot create** other API keys
- **Cannot revoke** API keys
- These operations require web session or basic auth

This prevents a compromised API key from being used to create persistent backdoor access.

## Encryption

### Database Credentials

Target database passwords are encrypted at rest using **AES-256-GCM**:

- 256-bit encryption keys
- Authenticated encryption (integrity + confidentiality)
- Random nonce per encryption operation
- **AAD binding**: Ciphertext is bound to the database ID, preventing credential transplant attacks

### Key Management

Encryption keys are provided via environment:

| Variable | Description |
|----------|-------------|
| `DBB_KEY` | Base64-encoded 32-byte key |
| `DBB_KEYFILE` | Path to file containing the key |

Keys are:
- Never logged or exposed via API
- Never transmitted over the network
- Required at startup (DBBat won't start without a valid key)

## Role-Based Access Control

### Roles

| Role | Description |
|------|-------------|
| `admin` | Full access to all resources and operations |
| `viewer` | Read-only access to observability data (queries, connections, audit) |
| `connector` | Can only connect to databases with active grants |

Users can have multiple roles. Permissions are additive.

### Resource Visibility by Role

| Resource | Admin | Viewer | Connector |
|----------|-------|--------|-----------|
| All users | Full | List only | Own only |
| All databases | Full details | Name/description | Granted only |
| All grants | Full | Full | Own only |
| All queries | Full | Full | None |
| All connections | Full | Full | Own only |
| Audit log | Full | Full | None |

## Access Grants

Grants control which users can connect to which databases through the proxy.

### Grant Constraints

| Constraint | Description |
|------------|-------------|
| `starts_at` | Grant is not valid before this time |
| `expires_at` | Grant automatically expires after this time |
| `max_query_counts` | Maximum queries allowed (quota) |
| `max_bytes_transferred` | Maximum data transfer allowed (quota) |
| `access_level` | `read` or `write` |

**Recommendation**: Always set all constraints. Time-limited grants with quotas minimize blast radius if credentials are compromised.

### Grant Lifecycle

1. Admin creates grant with constraints
2. Grant becomes active at `starts_at`
3. User can connect and execute queries
4. Quotas are enforced per-query
5. Grant expires at `expires_at` or when revoked
6. Revoked grants record `revoked_at` and `revoked_by` for audit

## Read-Only Mode

When a grant has `access_level: read`, DBBat enforces read-only access through **defense in depth**.

### Layer 1: Query Inspection

Queries are inspected and blocked if they match write patterns:

- **DML**: `INSERT`, `UPDATE`, `DELETE`, `MERGE`
- **DDL**: `CREATE`, `ALTER`, `DROP`, `TRUNCATE`
- **DCL**: `GRANT`, `REVOKE`
- **Other**: `COPY FROM`, `CALL` (procedures)

### Layer 2: PostgreSQL Session Setting

At connection establishment, DBBat sets:

```sql
SET SESSION default_transaction_read_only = on;
```

PostgreSQL itself then blocks any write operation, regardless of SQL syntax.

### Layer 3: Bypass Prevention

Attempts to disable read-only mode are blocked:

- `SET default_transaction_read_only = off`
- `RESET default_transaction_read_only`
- `SET SESSION AUTHORIZATION` (privilege escalation)
- `SET ROLE` (privilege escalation)

### Limitations

Read-only mode is **defense in depth for trusted users**, not a security boundary against malicious actors:

- Regex-based inspection may miss edge cases
- New PostgreSQL syntax could bypass detection
- Functions with `SECURITY DEFINER` might execute writes

**For untrusted access**: Create a dedicated PostgreSQL user with `GRANT SELECT` only.

## Audit Trail

### What's Logged

| Event Type | Data Captured |
|------------|---------------|
| Connections | User, database, source IP, timestamps, query count |
| Queries | SQL text, parameters, execution time, rows affected, errors |
| Query Results | All result rows (for replay/audit) |
| Access Changes | Grant creation, revocation, user changes |

### Audit Log Integrity

- Audit logs are append-only (no UPDATE/DELETE via API)
- Protected from modification via proxy (internal table protection)
- Includes `performed_by` for accountability

## API Rate Limiting

All authenticated endpoints are rate-limited:

- Per-user request limits
- Response headers indicate remaining quota
- `429 Too Many Requests` when exceeded

Rate limit exempt users can be configured for automation/CI.

## Network Security

### Upstream Connections

DBBat supports PostgreSQL SSL modes for upstream connections:

| Mode | Description |
|------|-------------|
| `disable` | No SSL |
| `prefer` | Try SSL, fall back to plain (default) |
| `require` | Require SSL, don't verify certificate |
| `verify-ca` | Require SSL, verify CA |
| `verify-full` | Require SSL, verify CA and hostname |

**Recommendation**: Use `require` or stronger for production.

### Client Connections

DBBat accepts plain PostgreSQL protocol connections. For encryption:

- Deploy behind a TLS-terminating load balancer, or
- Use network-level encryption (VPN, private network)

## Security Checklist

### Deployment

- [ ] Set strong encryption key (`DBB_KEY` or `DBB_KEYFILE`)
- [ ] Use separate database for DBBat storage
- [ ] Enable TLS for upstream connections (`ssl_mode: require`)
- [ ] Deploy in private network or behind VPN
- [ ] Change default admin password immediately

### Operations

- [ ] Use time-limited grants (hours/days, not years)
- [ ] Set query and byte quotas on all grants
- [ ] Prefer `read` access level unless writes are required
- [ ] Review audit logs regularly
- [ ] Rotate API keys periodically
- [ ] Monitor for blocked query attempts

### For Target Databases

- [ ] Use dedicated PostgreSQL user for DBBat upstream connections
- [ ] Grant minimum required privileges to that user
- [ ] For read-only grants, use PostgreSQL user with only SELECT privileges
- [ ] Enable PostgreSQL logging as additional audit layer
