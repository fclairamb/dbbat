# API Keys for PostgreSQL Proxy Authentication

## Goal

Allow users to authenticate to the PostgreSQL proxy using their API key (`dbb_...`) as the password, instead of their dbbat user password. This unifies the authentication mechanism across the REST API and the PG proxy, and is the first step toward never exposing upstream database credentials to clients.

The existing password authentication continues to work as a fallback.

## Prerequisites

- None (API keys and PG proxy auth already exist)

## Outcome

- PG proxy accepts API keys as passwords
- Username in StartupMessage must match the API key owner
- Grant/quota checks work identically to password auth
- Existing password auth is unaffected

## Non-Goals

- Oracle proxy changes (separate spec: `oracle-auth-termination.md`)
- API key scope restrictions (e.g., "this key can only be used for proxy")
- Deprecating password auth

---

## Test Mode: Pre-Provisioned API Keys

In test mode, stable API keys are pre-provisioned for each test user so that proxy authentication can be tested without generating keys first.

### Stable Test Keys

| User | API Key | Prefix |
|------|---------|--------|
| admin | `dbb_admin_key` | `dbb_admi` |
| connector | `dbb_connector_key` | `dbb_conn` |
| viewer | `dbb_viewer_key` | `dbb_view` |

These are shorter than production keys (which are 36 chars) but still valid — `VerifyAPIKey` only requires `len >= APIKeyPrefixLength` (8 chars).

### Store Method: `CreateAPIKeyWithValue`

A new store method to create an API key with a specific plaintext value (for test mode only):

```go
// CreateAPIKeyWithValue creates an API key with a specific plaintext value.
// Used for test mode provisioning where stable, predictable keys are needed.
func (s *Store) CreateAPIKeyWithValue(ctx context.Context, userID uuid.UUID, name string, plainKey string, expiresAt *time.Time) (*APIKey, error) {
    if len(plainKey) < APIKeyPrefixLength {
        return nil, fmt.Errorf("key too short: must be at least %d characters", APIKeyPrefixLength)
    }

    prefix := plainKey[:APIKeyPrefixLength]

    keyHash, err := crypto.HashPassword(plainKey)
    if err != nil {
        return nil, fmt.Errorf("failed to hash API key: %w", err)
    }

    apiKey := &APIKey{
        UserID:    userID,
        Name:      name,
        KeyHash:   keyHash,
        KeyPrefix: prefix,
        KeyType:   KeyTypeAPI,
        ExpiresAt: expiresAt,
        CreatedAt: time.Now(),
    }

    _, err = s.db.NewInsert().Model(apiKey).Returning("*").Exec(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to create API key: %w", err)
    }

    return apiKey, nil
}
```

### Test Provisioning Update

In `main.go`, `provisionTestData()` adds after creating users and grants:

```go
// 7. Create stable API keys for test users
testKeys := []struct {
    user    *store.User
    keyName string
    key     string
}{
    {adminUser, "admin-test-key", "dbb_admin_key"},
    {connectorUser, "connector-test-key", "dbb_connector_key"},
    {viewerUser, "viewer-test-key", "dbb_viewer_key"},
}
for _, tk := range testKeys {
    _, err := dataStore.CreateAPIKeyWithValue(ctx, tk.user.UID, tk.keyName, tk.key, nil)
    if err != nil {
        return fmt.Errorf("failed to create test API key for %s: %w", tk.user.Username, err)
    }
    logger.InfoContext(ctx, "Created test API key", slog.String("user", tk.user.Username), slog.String("key", tk.key))
}
```

---

## Design

### Authentication Flow (updated)

```
Client                          dbbat PG Proxy
  |                                |
  | StartupMessage(user=X, db=Y)  |
  |------------------------------->|
  |                                | Look up user X
  |                                | Look up database Y
  |                                | Check active grant (X → Y)
  |                                | Check quotas
  |                                |
  | AuthenticationCleartextPassword|
  |<-------------------------------|
  |                                |
  | PasswordMessage(password=P)    |
  |------------------------------->|
  |                                |
  |                                | if P starts with "dbb_":
  |                                |   VerifyAPIKey(P)
  |                                |   Check apiKey.UserID == user.UID
  |                                | else:
  |                                |   VerifyPassword(user.PasswordHash, P)
  |                                |
  | AuthenticationOk               |
  |<-------------------------------|
```

### Key Decision: Username Must Match

When authenticating with an API key, the username from the StartupMessage must match the API key owner's username. This prevents a user from impersonating another user with their own key. The user lookup and grant check happen based on the StartupMessage username (before password verification), same as today.

### Key Decision: API Key Prefix Detection

If the password starts with `dbb_` and has at least 8 characters (the prefix length), treat it as an API key attempt. If verification fails (wrong key, revoked, expired), return "authentication failed" immediately — do NOT fall through to password auth. This prevents timing-based information leakage about whether a `dbb_`-prefixed string is a valid password.

If the password does NOT start with `dbb_`, use existing password auth.

---

## Implementation

### File: `internal/proxy/postgresql/auth.go`

Update the `authenticate()` method. After receiving the password, add an API key branch:

```go
func (s *Session) authenticate() error {
    // ... existing: receive startup, look up user, database, grant, check quotas ...

    // Receive password (same as today)
    passwordMsg, err := s.receivePasswordMessage()
    if err != nil {
        return fmt.Errorf("failed to receive password: %w", err)
    }

    // NEW: Try API key authentication if password looks like an API key
    if isAPIKey(passwordMsg.Password) {
        if err := s.authenticateWithAPIKey(passwordMsg.Password); err != nil {
            s.sendError("authentication failed")
            return ErrInvalidPassword
        }
        s.authenticated = true
        return nil
    }

    // Existing password verification (unchanged)
    var valid bool
    if s.authCache != nil {
        valid, err = s.authCache.VerifyPassword(s.ctx, s.user.UID.String(), passwordMsg.Password, s.user.PasswordHash)
    } else {
        valid, err = crypto.VerifyPassword(s.user.PasswordHash, passwordMsg.Password)
    }
    if err != nil || !valid {
        s.sendError("authentication failed")
        return ErrInvalidPassword
    }

    s.authenticated = true
    return nil
}

// isAPIKey checks if a password looks like a dbbat API key.
func isAPIKey(password string) bool {
    return len(password) >= store.APIKeyPrefixLength && 
           strings.HasPrefix(password, store.APIKeyPrefix)
}

// authenticateWithAPIKey verifies the password as an API key and checks ownership.
func (s *Session) authenticateWithAPIKey(apiKey string) error {
    verified, err := s.store.VerifyAPIKey(s.ctx, apiKey)
    if err != nil {
        return fmt.Errorf("API key verification failed: %w", err)
    }

    // Ensure the API key belongs to the user from the StartupMessage
    if verified.UserID != s.user.UID {
        return fmt.Errorf("API key does not belong to user %s", s.user.Username)
    }

    // Increment usage asynchronously
    go s.store.IncrementAPIKeyUsage(context.Background(), verified.ID)

    return nil
}
```

No other files need changes. The store's `VerifyAPIKey` already handles prefix lookup, hash verification (with cache), revocation, and expiration checks.

---

## Tests

### File: `internal/proxy/postgresql/auth_test.go`

Add these test cases to the existing auth tests:

```go
func TestAuthenticate_WithAPIKey_Success(t *testing.T) {
    // Setup: create user, database, grant, and API key
    store := setupTestStore(t)
    user := createTestUser(t, store, "apiuser")
    db := createTestDatabase(t, store, "testdb")
    createTestGrant(t, store, user.UID, db.UID)
    _, plainKey, _ := store.CreateAPIKey(ctx, user.UID, "test-key", nil)

    // Connect with username=apiuser, password=<api_key>
    session := setupTestSession(t, store, "apiuser", "testdb")
    sendPassword(session, plainKey)

    assert.True(t, session.authenticated)
}

func TestAuthenticate_WithAPIKey_WrongUser(t *testing.T) {
    // API key belongs to user A, but StartupMessage says user B
    store := setupTestStore(t)
    userA := createTestUser(t, store, "usera")
    userB := createTestUser(t, store, "userb")
    db := createTestDatabase(t, store, "testdb")
    createTestGrant(t, store, userB.UID, db.UID)
    _, plainKey, _ := store.CreateAPIKey(ctx, userA.UID, "a-key", nil)

    session := setupTestSession(t, store, "userb", "testdb")
    sendPassword(session, plainKey)

    assert.False(t, session.authenticated)
}

func TestAuthenticate_WithAPIKey_Revoked(t *testing.T) {
    store := setupTestStore(t)
    user := createTestUser(t, store, "revokeduser")
    db := createTestDatabase(t, store, "testdb")
    createTestGrant(t, store, user.UID, db.UID)
    apiKey, plainKey, _ := store.CreateAPIKey(ctx, user.UID, "revoked-key", nil)
    store.RevokeAPIKey(ctx, apiKey.ID, user.UID)

    session := setupTestSession(t, store, "revokeduser", "testdb")
    sendPassword(session, plainKey)

    assert.False(t, session.authenticated)
}

func TestAuthenticate_WithAPIKey_Expired(t *testing.T) {
    store := setupTestStore(t)
    user := createTestUser(t, store, "expireduser")
    db := createTestDatabase(t, store, "testdb")
    createTestGrant(t, store, user.UID, db.UID)
    past := time.Now().Add(-time.Hour)
    _, plainKey, _ := store.CreateAPIKey(ctx, user.UID, "expired-key", &past)

    session := setupTestSession(t, store, "expireduser", "testdb")
    sendPassword(session, plainKey)

    assert.False(t, session.authenticated)
}

func TestAuthenticate_WithPassword_StillWorks(t *testing.T) {
    // Existing password auth is unaffected
    store := setupTestStore(t)
    user := createTestUser(t, store, "passuser")
    db := createTestDatabase(t, store, "testdb")
    createTestGrant(t, store, user.UID, db.UID)

    session := setupTestSession(t, store, "passuser", "testdb")
    sendPassword(session, "the-password")

    assert.True(t, session.authenticated)
}

func TestAuthenticate_WithDBBPrefix_NotAPIKey(t *testing.T) {
    // Password that happens to start with dbb_ but is not a valid key
    // Should fail (not fall through to password auth)
    store := setupTestStore(t)
    user := createTestUser(t, store, "trickuser")
    db := createTestDatabase(t, store, "testdb")
    createTestGrant(t, store, user.UID, db.UID)

    session := setupTestSession(t, store, "trickuser", "testdb")
    sendPassword(session, "dbb_this_is_not_a_real_key_at_all")

    assert.False(t, session.authenticated)
}
```

---

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/proxy/postgresql/auth.go` | Modified | Add API key auth branch + `authenticateWithAPIKey()` + `isAPIKey()` |
| `internal/proxy/postgresql/auth_test.go` | Modified | Add 6 test cases for API key auth |
| `internal/store/api_keys.go` | Modified | Add `CreateAPIKeyWithValue()` method |
| `main.go` | Modified | Provision stable API keys in test mode |

## Acceptance Criteria

1. `psql -U admin -d proxy_target` with `dbb_admin_key` as password → connects successfully (test mode)
2. `psql -U connector -d proxy_target` with `dbb_connector_key` as password → connects successfully
3. `psql -U admin -d proxy_target` with regular password `admintest` → still works
4. API key belonging to user A used with username B → authentication failed
5. Revoked API key → authentication failed
6. Expired API key → authentication failed
7. String starting with `dbb_` that's not a valid key → authentication failed (no fallback to password)
8. Grant checks and quota enforcement work identically with API key auth
9. API key usage counter is incremented on proxy auth
10. Stable test keys are provisioned on every test mode startup

## Estimated Size

~80 lines new code + ~100 lines tests = **~180 lines total**

## Implementation Plan

1. **Store: Add `CreateAPIKeyWithValue` method** — New method in `internal/store/api_keys.go` to create API keys with a specific plaintext value (for test mode provisioning)
2. **Proxy auth: Add API key authentication branch** — Update `internal/proxy/postgresql/auth.go` to detect `dbb_` prefixed passwords and verify them as API keys, with ownership check
3. **Test data: Provision stable API keys** — Update `main.go` `provisionTestData()` to create stable test API keys for admin, connector, and viewer users
4. **QA: Build, lint, test** — Ensure all checks pass
