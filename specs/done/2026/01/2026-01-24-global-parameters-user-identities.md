# Global Parameters and User Identities Tables

## Status: Draft

## Summary

Add two new tables to support external integrations (Slack, SSO providers): `global_parameters` for key-value configuration storage and `user_identities` for linking external identities to DBBat users.

## Problem

To support Slack integration and future SSO providers, we need:

1. **Configuration storage**: Store integration-specific settings like Slack team IDs, access tokens, signing secrets
2. **Identity linking**: Map external identities (Slack user IDs, SSO subject IDs) to DBBat users

Currently, there's no generic mechanism for either of these needs.

## Solution

### Global Parameters Table

A key-value store with grouping support for configuration:

```sql
CREATE TABLE global_parameters (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_key TEXT NOT NULL,           -- e.g., 'slack', 'oidc', 'saml'
    key TEXT NOT NULL,                 -- e.g., 'team_id', 'access_token'
    value TEXT NOT NULL,               -- Encrypted for sensitive values
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,            -- Soft delete

    CONSTRAINT unique_group_key UNIQUE (group_key, key) WHERE deleted_at IS NULL
);
CREATE INDEX idx_global_parameters_group_key ON global_parameters(group_key);
CREATE INDEX idx_global_parameters_deleted_at ON global_parameters(deleted_at) WHERE deleted_at IS NULL;
```

**Usage Examples:**

| group_key | key | value (example) |
|-----------|-----|-----------------|
| `slack` | `team_id` | `T0123456789` |
| `slack` | `access_token` | `xoxb-...` (encrypted) |
| `slack` | `signing_secret` | `abc123...` (encrypted) |
| `slack` | `bot_user_id` | `U0123ABC456` |
| `slack` | `default_channel_id` | `C0123DEF789` |

### User Identities Table

Links external provider identities to DBBat users:

```sql
CREATE TABLE user_identities (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_uid UUID NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    identity_type TEXT NOT NULL,       -- e.g., 'slack', 'google', 'github', 'saml'
    identity_value TEXT NOT NULL,      -- Provider-specific user ID
    metadata JSONB,                    -- Additional provider-specific data
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,            -- Soft delete

    CONSTRAINT unique_identity UNIQUE (identity_type, identity_value) WHERE deleted_at IS NULL
);
CREATE INDEX idx_user_identities_user_uid ON user_identities(user_uid);
CREATE INDEX idx_user_identities_type_value ON user_identities(identity_type, identity_value);
CREATE INDEX idx_user_identities_deleted_at ON user_identities(deleted_at) WHERE deleted_at IS NULL;
```

**Usage Examples:**

| user_uid | identity_type | identity_value | metadata |
|----------|---------------|----------------|----------|
| `abc-123` | `slack` | `U013ZGBT0SJ` | `{"team_id": "T0123456789", "display_name": "john.doe"}` |
| `abc-123` | `google` | `118234567890123456789` | `{"email": "john@example.com"}` |
| `def-456` | `slack` | `U024ABCD1EF` | `{"team_id": "T0123456789"}` |

## Implementation

### Go Models

**File**: `internal/store/models.go`

```go
// GlobalParameter represents a configuration parameter
type GlobalParameter struct {
    bun.BaseModel `bun:"table:global_parameters,alias:gp"`

    UID       uuid.UUID  `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
    GroupKey  string     `bun:"group_key,notnull" json:"group_key"`
    Key       string     `bun:"key,notnull" json:"key"`
    Value     string     `bun:"value,notnull" json:"value"`
    CreatedAt time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
    UpdatedAt time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
    DeletedAt *time.Time `bun:"deleted_at,soft_delete" json:"-"`
}

// UserIdentity links an external identity to a DBBat user
type UserIdentity struct {
    bun.BaseModel `bun:"table:user_identities,alias:ui"`

    UID           uuid.UUID       `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
    UserUID       uuid.UUID       `bun:"user_uid,notnull,type:uuid" json:"user_uid"`
    IdentityType  string          `bun:"identity_type,notnull" json:"identity_type"`
    IdentityValue string          `bun:"identity_value,notnull" json:"identity_value"`
    Metadata      json.RawMessage `bun:"metadata,type:jsonb" json:"metadata,omitempty"`
    CreatedAt     time.Time       `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
    UpdatedAt     time.Time       `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
    DeletedAt     *time.Time      `bun:"deleted_at,soft_delete" json:"-"`

    // Relations
    User *User `bun:"rel:belongs-to,join:user_uid=uid" json:"user,omitempty"`
}

// Identity type constants
const (
    IdentityTypeSlack  = "slack"
    IdentityTypeGoogle = "google"
    IdentityTypeGitHub = "github"
    IdentityTypeSAML   = "saml"
)
```

### Store Methods

**File**: `internal/store/global_parameters.go`

```go
// GetParameter retrieves a single parameter by group and key
func (s *Store) GetParameter(ctx context.Context, groupKey, key string) (*GlobalParameter, error)

// GetParameters retrieves all parameters for a group
func (s *Store) GetParameters(ctx context.Context, groupKey string) ([]GlobalParameter, error)

// SetParameter creates or updates a parameter
func (s *Store) SetParameter(ctx context.Context, groupKey, key, value string) error

// DeleteParameter soft-deletes a parameter
func (s *Store) DeleteParameter(ctx context.Context, groupKey, key string) error
```

**File**: `internal/store/user_identities.go`

```go
// GetUserByIdentity finds a user by their external identity
func (s *Store) GetUserByIdentity(ctx context.Context, identityType, identityValue string) (*User, error)

// GetUserIdentity retrieves a specific identity
func (s *Store) GetUserIdentity(ctx context.Context, identityType, identityValue string) (*UserIdentity, error)

// GetUserIdentities retrieves all identities for a user
func (s *Store) GetUserIdentities(ctx context.Context, userUID uuid.UUID) ([]UserIdentity, error)

// CreateUserIdentity links an external identity to a user
func (s *Store) CreateUserIdentity(ctx context.Context, identity *UserIdentity) error

// DeleteUserIdentity removes an identity link
func (s *Store) DeleteUserIdentity(ctx context.Context, identityType, identityValue string) error
```

### Encryption for Sensitive Values

Sensitive parameters (tokens, secrets) should be encrypted using the existing AES-256-GCM encryption from `internal/crypto`. The store methods should accept an optional encryption flag or use a naming convention (e.g., keys ending in `_token` or `_secret` are encrypted).

**Recommended approach**: Use a prefix convention for encrypted values:

```go
const encryptedPrefix = "enc:"

func (s *Store) SetParameterEncrypted(ctx context.Context, groupKey, key, value string) error {
    encrypted, err := s.crypto.Encrypt([]byte(value))
    if err != nil {
        return err
    }
    return s.SetParameter(ctx, groupKey, key, encryptedPrefix + base64.StdEncoding.EncodeToString(encrypted))
}

func (s *Store) GetParameterDecrypted(ctx context.Context, groupKey, key string) (string, error) {
    param, err := s.GetParameter(ctx, groupKey, key)
    if err != nil {
        return "", err
    }
    if !strings.HasPrefix(param.Value, encryptedPrefix) {
        return param.Value, nil
    }
    encrypted, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(param.Value, encryptedPrefix))
    if err != nil {
        return "", err
    }
    decrypted, err := s.crypto.Decrypt(encrypted)
    if err != nil {
        return "", err
    }
    return string(decrypted), nil
}
```

## Migration

**File**: `internal/migrations/sql/20260124000000_global_parameters_user_identities.up.sql`

```sql
-- Global parameters for integration configuration
CREATE TABLE global_parameters (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_key TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_global_parameters_group_key ON global_parameters(group_key);
CREATE UNIQUE INDEX idx_global_parameters_unique ON global_parameters(group_key, key) WHERE deleted_at IS NULL;
CREATE INDEX idx_global_parameters_deleted_at ON global_parameters(deleted_at) WHERE deleted_at IS NULL;

--bun:split

-- User identities for external provider linking
CREATE TABLE user_identities (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_uid UUID NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    identity_type TEXT NOT NULL,
    identity_value TEXT NOT NULL,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_user_identities_user_uid ON user_identities(user_uid);
CREATE INDEX idx_user_identities_type_value ON user_identities(identity_type, identity_value);
CREATE UNIQUE INDEX idx_user_identities_unique ON user_identities(identity_type, identity_value) WHERE deleted_at IS NULL;
CREATE INDEX idx_user_identities_deleted_at ON user_identities(deleted_at) WHERE deleted_at IS NULL;
```

**File**: `internal/migrations/sql/20260124000000_global_parameters_user_identities.down.sql`

```sql
DROP TABLE IF EXISTS user_identities;

--bun:split

DROP TABLE IF EXISTS global_parameters;
```

## Testing Requirements

### Unit Tests

```go
func TestGlobalParameters(t *testing.T) {
    t.Run("set and get parameter", func(t *testing.T) {
        // Set a parameter
        // Get it back
        // Verify value matches
    })

    t.Run("update existing parameter", func(t *testing.T) {
        // Set a parameter
        // Set same group/key with different value
        // Verify updated value
    })

    t.Run("get all parameters for group", func(t *testing.T) {
        // Set multiple parameters in same group
        // Get all for group
        // Verify all returned
    })

    t.Run("encrypted parameters", func(t *testing.T) {
        // Set encrypted parameter
        // Get decrypted value
        // Verify original value
    })

    t.Run("soft delete", func(t *testing.T) {
        // Set parameter
        // Delete it
        // Verify not found
        // Verify can create new with same key
    })
}

func TestUserIdentities(t *testing.T) {
    t.Run("create and get identity", func(t *testing.T) {
        // Create user
        // Create identity
        // Get by identity type/value
        // Verify user returned
    })

    t.Run("unique constraint", func(t *testing.T) {
        // Create identity
        // Try to create duplicate
        // Verify error
    })

    t.Run("user can have multiple identities", func(t *testing.T) {
        // Create user
        // Add Slack identity
        // Add Google identity
        // Get all identities for user
        // Verify both returned
    })

    t.Run("cascade delete on user delete", func(t *testing.T) {
        // Create user with identity
        // Delete user
        // Verify identity also deleted
    })
}
```

## Security Considerations

1. **Encryption at rest**: Sensitive parameters (tokens, secrets) are encrypted using AES-256-GCM
2. **Access control**: Only admin users can read/write global parameters via API
3. **Audit logging**: All parameter changes should be logged
4. **No API exposure for identities**: User identities are internal only, not exposed via REST API
5. **Soft delete**: Preserves history for audit purposes

## Future Extensions

- **TTL support**: Add `expires_at` column to global_parameters for token rotation
- **Versioning**: Track parameter history with a separate `global_parameter_history` table
- **Namespacing**: Support multi-tenant deployments with organization-scoped parameters
