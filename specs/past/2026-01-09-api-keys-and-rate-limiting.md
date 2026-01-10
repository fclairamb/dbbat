# API Keys and Rate Limiting

## Overview

Add API key authentication as an alternative to Basic Auth, along with rate limiting to protect against brute force attacks and abuse.

## Motivation

- **API Keys**: Provide a more secure authentication method for automation/scripts that doesn't require storing user passwords. Keys can be rotated independently, have expiration dates, and be revoked without changing the user's password.
- **Rate Limiting**: Protect the API against brute force attacks and abuse by limiting request rates per user/IP.

## Design

### API Keys

#### Database Schema

Add a new `api_keys` table:

```sql
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    key_hash VARCHAR(255) NOT NULL,  -- Argon2id hash of the key
    key_prefix VARCHAR(8) NOT NULL,  -- First 8 chars for identification (e.g., "dbb_a1b2")
    expires_at TIMESTAMPTZ,          -- NULL = never expires
    last_used_at TIMESTAMPTZ,
    request_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users(id),

    CONSTRAINT unique_key_prefix UNIQUE (key_prefix)
);

CREATE INDEX idx_api_keys_user_id ON api_keys(user_id);
CREATE INDEX idx_api_keys_key_prefix ON api_keys(key_prefix);
```

#### Key Format

API keys follow a predictable format for easy identification:

```
dbb_<random_32_chars>
```

Example: `dbb_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3`

- Prefix `dbb_` identifies it as a DBBat API key
- 32 random alphanumeric characters (a-z, 0-9)
- Total length: 36 characters
- The first 8 characters (including prefix) are stored as `key_prefix` for identification

#### Key Storage

- The full key is shown to the user **only once** at creation time
- The key is hashed using Argon2id (same as passwords) before storage
- Only the `key_prefix` is stored in plaintext for identification purposes

#### API Key Restrictions

Regular API keys (`dbb_` prefix) have the following restrictions compared to web session authentication:

1. **Cannot create/delete API keys** - Must use web session auth to manage API keys (prevents key escalation)
2. **Cannot modify own user account** - API keys cannot update the user they belong to

**Web session keys** (`web_` prefix) have fewer restrictions:
- Can create and revoke API keys (user is interactively logged in)

**Password changes** do NOT use any key (web or API):
- Password change endpoints require username + current password in the request body
- This ensures the user proves knowledge of their current password
- See the Authentication Flow spec for details

#### Authentication Flow

1. Client sends request with header: `Authorization: Bearer dbb_k7x9m2p4...`
2. Server extracts the key prefix (`dbb_k7x9`) to find potential matches
3. Server verifies the full key against stored hashes
4. If valid and not expired/revoked:
   - Update `last_used_at` and increment `request_count`
   - Process request with the key's associated user context
5. If invalid: return `401 Unauthorized`

#### API Endpoints

##### Create API Key
```
POST /api/keys
Authorization: Bearer web_<token>  (web session key required, regular API keys NOT allowed)

Request:
{
    "name": "CI/CD Pipeline",
    "expires_at": "2026-06-01T00:00:00Z"  // optional
}

Response (201 Created):
{
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "name": "CI/CD Pipeline",
    "key": "dbb_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3",  // Only shown once!
    "key_prefix": "dbb_k7x9",
    "expires_at": "2026-06-01T00:00:00Z",
    "created_at": "2026-01-09T12:00:00Z"
}
```

**Important**: The full `key` is only returned in this response. It cannot be retrieved later.

**Authentication**: Only web session keys (`web_` prefix) can create API keys. Regular API keys (`dbb_` prefix) cannot create new keys (prevents key escalation).

##### List API Keys
```
GET /api/keys
Authorization: Basic <user:password> OR Bearer <api_key>

Response (200 OK):
{
    "keys": [
        {
            "id": "550e8400-e29b-41d4-a716-446655440000",
            "name": "CI/CD Pipeline",
            "key_prefix": "dbb_k7x9",
            "expires_at": "2026-06-01T00:00:00Z",
            "last_used_at": "2026-01-09T10:30:00Z",
            "request_count": 1547,
            "created_at": "2026-01-09T12:00:00Z",
            "revoked_at": null
        }
    ]
}
```

- Non-admin users see only their own keys
- Admin users see all keys (can filter by `?user_id=<uuid>`)

##### Get API Key
```
GET /api/keys/:id
Authorization: Basic <user:password> OR Bearer <api_key>

Response (200 OK):
{
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "user_id": "123e4567-e89b-12d3-a456-426614174000",
    "name": "CI/CD Pipeline",
    "key_prefix": "dbb_k7x9",
    "expires_at": "2026-06-01T00:00:00Z",
    "last_used_at": "2026-01-09T10:30:00Z",
    "request_count": 1547,
    "created_at": "2026-01-09T12:00:00Z",
    "revoked_at": null
}
```

##### Revoke API Key
```
DELETE /api/keys/:id
Authorization: Bearer web_<token>  (web session key required, regular API keys NOT allowed)

Response (204 No Content)
```

- Sets `revoked_at` timestamp and `revoked_by` user ID
- Key is immediately invalidated
- Audit log entry created

**Authentication**: Only web session keys (`web_` prefix) can revoke API keys. Regular API keys (`dbb_` prefix) cannot revoke keys.

#### Audit Log

API key operations generate audit log entries:

| Action | Details |
|--------|---------|
| `api_key.created` | Key created (includes key name, prefix, user_id) |
| `api_key.revoked` | Key revoked (includes key name, prefix, revoked_by) |

**Important**: All audit log entries MUST include the `key_id` and `key_type` fields to track which authentication method was used:

```json
{
    "user_id": "550e8400-e29b-41d4-a716-446655440000",
    "key_id": "660e8400-e29b-41d4-a716-446655440001",
    "key_type": "web",
    "action": "api_key.created",
    "resource_type": "api_key",
    "resource_id": "770e8400-e29b-41d4-a716-446655440002",
    "details": {"name": "CI/CD Pipeline", "key_prefix": "dbb_k7x9"},
    "ip_address": "192.168.1.100"
}
```

This allows:
- Tracing all actions performed with a specific key
- Identifying compromised keys by reviewing their activity
- Compliance with security audit requirements

### Rate Limiting

#### Algorithm

Use a **sliding window** algorithm with in-memory storage

#### Configuration

Add to configuration (koanf):

```yaml
rate_limit:
  enabled: true
  requests_per_minute: 60      # Per authenticated user
  requests_per_minute_anon: 10 # Per IP for unauthenticated requests
  burst: 10                    # Allow short bursts above the limit
```

Environment variables:
- `DBB_RATE_LIMIT_ENABLED` (default: `true`)
- `DBB_RATE_LIMIT_RPM` (default: `60`)
- `DBB_RATE_LIMIT_RPM_ANON` (default: `10`)
- `DBB_RATE_LIMIT_BURST` (default: `10`)

#### Implementation

Rate limiting is applied per:
1. **Authenticated requests**: By user ID (regardless of auth method)
2. **Unauthenticated requests**: By source IP

#### Response Headers

All responses include rate limit headers:

```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 45
X-RateLimit-Reset: 1704812400
```

#### Rate Limit Exceeded

When limit is exceeded, return:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 30
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1704812400

{
    "error": "rate_limit_exceeded",
    "message": "Too many requests. Please retry after 30 seconds.",
    "retry_after": 30
}
```

#### Exclusions

Rate limiting can be disabled for specific users (e.g., service accounts) by adding a `rate_limit_exempt` boolean to the users table:

```sql
ALTER TABLE users ADD COLUMN rate_limit_exempt BOOLEAN NOT NULL DEFAULT FALSE;
```

## Migration

Add to `internal/migrations/sql/`:

**20260109000000_api_keys_rate_limiting.up.sql**:
```sql
-- API Keys table
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    key_hash VARCHAR(255) NOT NULL,
    key_prefix VARCHAR(8) NOT NULL,
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    request_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users(id),

    CONSTRAINT unique_key_prefix UNIQUE (key_prefix)
);

CREATE INDEX idx_api_keys_user_id ON api_keys(user_id);
CREATE INDEX idx_api_keys_key_prefix ON api_keys(key_prefix);

--bun:split

-- Rate limit exemption for users
ALTER TABLE users ADD COLUMN rate_limit_exempt BOOLEAN NOT NULL DEFAULT FALSE;
```

**20260109000000_api_keys_rate_limiting.down.sql**:
```sql
ALTER TABLE users DROP COLUMN rate_limit_exempt;

--bun:split

DROP TABLE api_keys;
```

## Implementation Plan

### Phase 1: API Keys

1. Add migration for `api_keys` table
2. Add `APIKey` model in `internal/store/models.go`
3. Create `internal/store/api_keys.go`:
   - `CreateAPIKey(userID uuid.UUID, name string, expiresAt *time.Time) (*APIKey, plainKey string, error)`
   - `GetAPIKeyByPrefix(prefix string) (*APIKey, error)`
   - `ListAPIKeys(userID *uuid.UUID) ([]APIKey, error)`
   - `RevokeAPIKey(id uuid.UUID, revokedBy uuid.UUID) error`
   - `IncrementAPIKeyUsage(id uuid.UUID) error`
4. Update `internal/api/middleware.go`:
   - Add Bearer token parsing
   - Verify API key and load user context
   - Track key type (`web` vs `api`) in context
   - Track restrictions based on key type
5. Create `internal/api/keys.go`:
   - `POST /api/keys` - Create key (web session only)
   - `GET /api/keys` - List keys
   - `GET /api/keys/:id` - Get key details
   - `DELETE /api/keys/:id` - Revoke key (web session only)
6. Add `requireWebSession()` middleware for endpoints that require web session auth
7. Add audit log entries for key operations
8. Update OpenAPI spec

### Phase 2: Rate Limiting

1. Add configuration options to koanf config
2. Create `internal/api/ratelimit.go`:
   - Sliding window implementation
   - In-memory storage with cleanup goroutine
3. Add rate limit middleware to Gin router
4. Add response headers
5. Add `rate_limit_exempt` column to users
6. Update OpenAPI spec with 429 responses

## Security Considerations

1. **Key hashing**: API keys are hashed with Argon2id, same as passwords. A database dump does not expose usable credentials.

2. **Key restrictions**: API keys cannot perform sensitive operations (password changes, key management) to limit blast radius if compromised.

3. **Audit trail**: All key operations are logged for security review.

4. **Expiration**: Keys can have expiration dates for automatic rotation enforcement.

5. **Rate limiting**: Protects against brute force attacks on both auth endpoints and API key guessing.

6. **Key prefix**: Only 8 characters are stored in plaintext, making enumeration attacks impractical while still allowing key identification in logs.

## Testing

1. **Unit tests**:
   - Key generation and hashing
   - Key verification
   - Rate limit algorithm

2. **Integration tests** (testcontainers):
   - Key CRUD operations
   - Key authentication flow
   - Key restrictions enforcement
   - Rate limit enforcement

3. **Manual testing**:
   - Create key, verify it works
   - Verify key restrictions
   - Test rate limiting with `ab` or similar tool
