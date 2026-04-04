# Slack Auth Phase 1: User Identities & OAuth State

> Part of: Slack Authentication series

## Goal

Add the foundational database tables and store methods needed to link external identities (starting with Slack) to DBBat users, and to manage OAuth state during the authorization flow. This is the prerequisite for the "Sign in with Slack" feature.

## Prerequisites

- None (this is the first phase)

## Outcome

- `user_identities` table and store CRUD methods
- `oauth_states` table for CSRF protection during OAuth flow
- SQL migration (up + down)
- Unit tests for all store methods

## Non-Goals

- No API endpoints yet (Phase 2)
- No Slack-specific logic (Phase 2)
- No frontend changes (Phase 3)
- No `global_parameters` table (use environment variables for now; simpler, and the existing spec can be implemented later if needed)

---

## Database Schema

### user_identities

Links an external provider identity to a DBBat user. One user can have multiple identities (e.g., Slack + Google), but each provider identity maps to exactly one user.

```sql
-- Migration: YYYYMMDDHHMMSS_user_identities.up.sql

CREATE TABLE user_identities (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_uid UUID NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    identity_type TEXT NOT NULL,       -- 'slack', 'google', 'github', 'saml'
    identity_value TEXT NOT NULL,      -- Provider-specific user ID (e.g., Slack user ID 'U013ZGBT0SJ')
    email TEXT,                        -- Email from provider (for matching existing users)
    display_name TEXT,                 -- Display name from provider
    metadata JSONB,                    -- Additional provider-specific data
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ            -- Soft delete
);

CREATE UNIQUE INDEX idx_user_identities_unique
    ON user_identities(identity_type, identity_value)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_user_identities_user_uid ON user_identities(user_uid);
CREATE INDEX idx_user_identities_email ON user_identities(email) WHERE email IS NOT NULL;
```

### oauth_states

Stores ephemeral state for OAuth CSRF protection. Each entry lives for a few minutes and is consumed on callback.

```sql
CREATE TABLE oauth_states (
    state TEXT PRIMARY KEY,            -- Random string sent to OAuth provider
    provider TEXT NOT NULL,            -- 'slack'
    redirect_uri TEXT,                 -- Where to redirect after login
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL    -- Typically NOW() + 10 minutes
);

CREATE INDEX idx_oauth_states_expires ON oauth_states(expires_at);

-- Down migration
DROP TABLE IF EXISTS oauth_states;

--bun:split

DROP TABLE IF EXISTS user_identities;
```

## Go Models

**File**: `internal/store/models.go`

```go
// Identity type constants
const (
    IdentityTypeSlack = "slack"
)

// UserIdentity links an external identity to a DBBat user.
type UserIdentity struct {
    bun.BaseModel `bun:"table:user_identities,alias:ui"`

    UID           uuid.UUID       `bun:"uid,pk,type:uuid,default:gen_random_uuid()" json:"uid"`
    UserUID       uuid.UUID       `bun:"user_uid,notnull,type:uuid" json:"user_uid"`
    IdentityType  string          `bun:"identity_type,notnull" json:"identity_type"`
    IdentityValue string          `bun:"identity_value,notnull" json:"identity_value"`
    Email         string          `bun:"email" json:"email,omitempty"`
    DisplayName   string          `bun:"display_name" json:"display_name,omitempty"`
    Metadata      json.RawMessage `bun:"metadata,type:jsonb" json:"metadata,omitempty"`
    CreatedAt     time.Time       `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
    UpdatedAt     time.Time       `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
    DeletedAt     *time.Time      `bun:"deleted_at,soft_delete" json:"-"`

    User *User `bun:"rel:belongs-to,join:user_uid=uid" json:"user,omitempty"`
}

// OAuthState stores ephemeral state for OAuth CSRF protection.
type OAuthState struct {
    bun.BaseModel `bun:"table:oauth_states,alias:os"`

    State       string    `bun:"state,pk" json:"state"`
    Provider    string    `bun:"provider,notnull" json:"provider"`
    RedirectURI string    `bun:"redirect_uri" json:"redirect_uri,omitempty"`
    CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
    ExpiresAt   time.Time `bun:"expires_at,notnull" json:"expires_at"`
}
```

## Store Methods

**File**: `internal/store/user_identities.go`

```go
// GetUserByIdentity finds a user by their external identity.
func (s *Store) GetUserByIdentity(ctx context.Context, identityType, identityValue string) (*User, error)

// GetUserIdentity retrieves a specific identity link.
func (s *Store) GetUserIdentity(ctx context.Context, identityType, identityValue string) (*UserIdentity, error)

// GetUserIdentities retrieves all identities for a user.
func (s *Store) GetUserIdentities(ctx context.Context, userUID uuid.UUID) ([]UserIdentity, error)

// CreateUserIdentity links an external identity to a user.
func (s *Store) CreateUserIdentity(ctx context.Context, identity *UserIdentity) (*UserIdentity, error)

// DeleteUserIdentity removes an identity link (soft delete).
func (s *Store) DeleteUserIdentity(ctx context.Context, uid uuid.UUID) error
```

**File**: `internal/store/oauth_states.go`

```go
// CreateOAuthState stores a new OAuth state for CSRF protection.
func (s *Store) CreateOAuthState(ctx context.Context, state *OAuthState) error

// ConsumeOAuthState retrieves and deletes an OAuth state (one-time use).
// Returns ErrOAuthStateNotFound if not found or expired.
func (s *Store) ConsumeOAuthState(ctx context.Context, state string) (*OAuthState, error)

// CleanupExpiredOAuthStates removes states older than their expiry. Called periodically.
func (s *Store) CleanupExpiredOAuthStates(ctx context.Context) (int64, error)
```

## Tests

**File**: `internal/store/user_identities_test.go`

```go
func TestUserIdentities(t *testing.T) {
    store := setupTestStore(t)
    ctx := context.Background()

    // Create a test user
    user, _ := store.CreateUser(ctx, "testuser", "hash", []string{RoleConnector})

    t.Run("create and get identity", func(t *testing.T) {
        identity := &UserIdentity{
            UserUID:       user.UID,
            IdentityType:  IdentityTypeSlack,
            IdentityValue: "U013ZGBT0SJ",
            Email:         "test@example.com",
            DisplayName:   "Test User",
        }
        created, err := store.CreateUserIdentity(ctx, identity)
        require.NoError(t, err)
        assert.NotEqual(t, uuid.Nil, created.UID)

        found, err := store.GetUserIdentity(ctx, IdentityTypeSlack, "U013ZGBT0SJ")
        require.NoError(t, err)
        assert.Equal(t, user.UID, found.UserUID)
    })

    t.Run("get user by identity", func(t *testing.T) {
        found, err := store.GetUserByIdentity(ctx, IdentityTypeSlack, "U013ZGBT0SJ")
        require.NoError(t, err)
        assert.Equal(t, "testuser", found.Username)
    })

    t.Run("duplicate identity rejected", func(t *testing.T) {
        duplicate := &UserIdentity{
            UserUID:       user.UID,
            IdentityType:  IdentityTypeSlack,
            IdentityValue: "U013ZGBT0SJ",
        }
        _, err := store.CreateUserIdentity(ctx, duplicate)
        assert.Error(t, err)
    })

    t.Run("user can have multiple identity types", func(t *testing.T) {
        google := &UserIdentity{
            UserUID:       user.UID,
            IdentityType:  "google",
            IdentityValue: "118234567890",
        }
        _, err := store.CreateUserIdentity(ctx, google)
        require.NoError(t, err)

        identities, err := store.GetUserIdentities(ctx, user.UID)
        require.NoError(t, err)
        assert.Len(t, identities, 2)
    })

    t.Run("cascade delete on user delete", func(t *testing.T) {
        tempUser, _ := store.CreateUser(ctx, "temp", "hash", []string{RoleConnector})
        _, _ = store.CreateUserIdentity(ctx, &UserIdentity{
            UserUID: tempUser.UID, IdentityType: IdentityTypeSlack, IdentityValue: "UTEMP",
        })
        _ = store.DeleteUser(ctx, tempUser.UID)
        _, err := store.GetUserIdentity(ctx, IdentityTypeSlack, "UTEMP")
        assert.Error(t, err)
    })
}
```

**File**: `internal/store/oauth_states_test.go`

```go
func TestOAuthStates(t *testing.T) {
    store := setupTestStore(t)
    ctx := context.Background()

    t.Run("create and consume state", func(t *testing.T) {
        state := &OAuthState{
            State:    "random-state-123",
            Provider: "slack",
            ExpiresAt: time.Now().Add(10 * time.Minute),
        }
        err := store.CreateOAuthState(ctx, state)
        require.NoError(t, err)

        consumed, err := store.ConsumeOAuthState(ctx, "random-state-123")
        require.NoError(t, err)
        assert.Equal(t, "slack", consumed.Provider)

        // Second consume should fail
        _, err = store.ConsumeOAuthState(ctx, "random-state-123")
        assert.Error(t, err)
    })

    t.Run("expired state rejected", func(t *testing.T) {
        state := &OAuthState{
            State:    "expired-state",
            Provider: "slack",
            ExpiresAt: time.Now().Add(-1 * time.Minute), // already expired
        }
        _ = store.CreateOAuthState(ctx, state)

        _, err := store.ConsumeOAuthState(ctx, "expired-state")
        assert.Error(t, err)
    })

    t.Run("cleanup expired states", func(t *testing.T) {
        for i := 0; i < 5; i++ {
            _ = store.CreateOAuthState(ctx, &OAuthState{
                State:    fmt.Sprintf("old-%d", i),
                Provider: "slack",
                ExpiresAt: time.Now().Add(-1 * time.Hour),
            })
        }
        deleted, err := store.CleanupExpiredOAuthStates(ctx)
        require.NoError(t, err)
        assert.GreaterOrEqual(t, deleted, int64(5))
    })
}
```

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/store/models.go` | Modified | Add `UserIdentity`, `OAuthState`, `IdentityTypeSlack` |
| `internal/store/user_identities.go` | New | CRUD for user identity links |
| `internal/store/user_identities_test.go` | New | Tests |
| `internal/store/oauth_states.go` | New | OAuth state management |
| `internal/store/oauth_states_test.go` | New | Tests |
| `internal/migrations/sql/YYYYMMDDHHMMSS_user_identities.up.sql` | New | Migration |
| `internal/migrations/sql/YYYYMMDDHHMMSS_user_identities.down.sql` | New | Rollback |

## Acceptance Criteria

1. `user_identities` table created with unique constraint on `(identity_type, identity_value)`
2. `oauth_states` table created for CSRF state management
3. `GetUserByIdentity("slack", "U013...")` returns the linked user
4. `ConsumeOAuthState` returns the state exactly once, then rejects
5. Expired OAuth states are rejected
6. Deleting a user cascades to delete their identities
7. All store methods have passing tests
8. Existing functionality unaffected (no regressions)

## Estimated Size

~200 lines Go code + ~150 lines tests + ~30 lines SQL = **~380 lines total**
