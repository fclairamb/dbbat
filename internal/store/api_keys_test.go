package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
)

func TestGenerateAPIKey(t *testing.T) {
	t.Run("generates valid key format", func(t *testing.T) {
		fullKey, prefix, err := generateAPIKey()
		if err != nil {
			t.Fatalf("generateAPIKey() error = %v", err)
		}

		// Check key format
		if !strings.HasPrefix(fullKey, APIKeyPrefix) {
			t.Errorf("generateAPIKey() fullKey should start with %q, got %q", APIKeyPrefix, fullKey)
		}

		// Check key length (dbb_ + 32 random chars = 36)
		if len(fullKey) != 36 {
			t.Errorf("generateAPIKey() fullKey length = %d, want 36", len(fullKey))
		}

		// Check prefix length
		if len(prefix) != APIKeyPrefixLength {
			t.Errorf("generateAPIKey() prefix length = %d, want %d", len(prefix), APIKeyPrefixLength)
		}

		// Check prefix matches beginning of full key
		if !strings.HasPrefix(fullKey, prefix) {
			t.Errorf("generateAPIKey() fullKey should start with prefix %q", prefix)
		}
	})

	t.Run("generates unique keys", func(t *testing.T) {
		keys := make(map[string]bool)
		for i := 0; i < 100; i++ {
			fullKey, _, err := generateAPIKey()
			if err != nil {
				t.Fatalf("generateAPIKey() error = %v", err)
			}
			if keys[fullKey] {
				t.Errorf("generateAPIKey() generated duplicate key")
			}
			keys[fullKey] = true
		}
	})
}

func TestCreateAPIKey(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a test user
	user, err := store.CreateUser(ctx, "apiuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("create key without expiration", func(t *testing.T) {
		apiKey, plainKey, err := store.CreateAPIKey(ctx, user.UID, "Test Key", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		if apiKey.ID == uuid.Nil {
			t.Error("CreateAPIKey() apiKey.ID = uuid.Nil")
		}
		if apiKey.UserID != user.UID {
			t.Errorf("CreateAPIKey() apiKey.UserID = %s, want %s", apiKey.UserID, user.UID)
		}
		if apiKey.Name != "Test Key" {
			t.Errorf("CreateAPIKey() apiKey.Name = %q, want %q", apiKey.Name, "Test Key")
		}
		if apiKey.ExpiresAt != nil {
			t.Errorf("CreateAPIKey() apiKey.ExpiresAt = %v, want nil", apiKey.ExpiresAt)
		}
		if apiKey.CreatedAt.IsZero() {
			t.Error("CreateAPIKey() apiKey.CreatedAt is zero")
		}
		if apiKey.RevokedAt != nil {
			t.Errorf("CreateAPIKey() apiKey.RevokedAt = %v, want nil", apiKey.RevokedAt)
		}

		// Check plain key format
		if !strings.HasPrefix(plainKey, APIKeyPrefix) {
			t.Errorf("CreateAPIKey() plainKey should start with %q", APIKeyPrefix)
		}
		if len(plainKey) != 36 {
			t.Errorf("CreateAPIKey() plainKey length = %d, want 36", len(plainKey))
		}

		// Check key prefix matches
		if !strings.HasPrefix(plainKey, apiKey.KeyPrefix) {
			t.Errorf("CreateAPIKey() plainKey should start with prefix %q", apiKey.KeyPrefix)
		}
	})

	t.Run("create key with expiration", func(t *testing.T) {
		expiresAt := time.Now().Add(24 * time.Hour)
		apiKey, _, err := store.CreateAPIKey(ctx, user.UID, "Expiring Key", &expiresAt)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		if apiKey.ExpiresAt == nil {
			t.Error("CreateAPIKey() apiKey.ExpiresAt = nil, expected a value")
		} else if apiKey.ExpiresAt.Sub(expiresAt) > time.Second {
			t.Errorf("CreateAPIKey() apiKey.ExpiresAt = %v, want %v", apiKey.ExpiresAt, expiresAt)
		}
	})
}

func TestAPIKeyProtocolDataRoundTrip(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "oracleuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	// 32-byte AES-256 master key so the O5LOGON verifiers are computed + encrypted
	// and stored in the protocol_data jsonb column.
	encKey := bytes.Repeat([]byte{0x42}, 32)

	created, _, err := store.CreateAPIKey(ctx, user.UID, "Oracle Key", nil, encKey)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}

	od := created.OracleData()
	if od == nil {
		t.Fatal("created.OracleData() = nil, want Oracle protocol data")
	}
	if len(od.O5LogonSalt6949) == 0 || len(od.O5LogonVerifier6949) == 0 ||
		len(od.O5LogonSalt18453) == 0 || len(od.O5LogonVerifier18453) == 0 {
		t.Fatalf("created O5LOGON material incomplete: %+v", od)
	}

	// Round-trip through the jsonb column: bun must marshal []byte as base64 and
	// recover the exact bytes on read.
	fetched, err := store.GetAPIKeyByPrefix(ctx, created.KeyPrefix)
	if err != nil {
		t.Fatalf("GetAPIKeyByPrefix() error = %v", err)
	}

	fod := fetched.OracleData()
	if fod == nil {
		t.Fatal("fetched.OracleData() = nil after jsonb round-trip")
	}

	for _, tc := range []struct {
		name      string
		got, want []byte
	}{
		{"salt_6949", fod.O5LogonSalt6949, od.O5LogonSalt6949},
		{"verifier_6949", fod.O5LogonVerifier6949, od.O5LogonVerifier6949},
		{"salt_18453", fod.O5LogonSalt18453, od.O5LogonSalt18453},
		{"verifier_18453", fod.O5LogonVerifier18453, od.O5LogonVerifier18453},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("%s mismatch after round-trip:\n got=%x\nwant=%x", tc.name, tc.got, tc.want)
		}
	}

	// The round-tripped verifiers must still decrypt with the master key + AAD —
	// i.e. the bytes survived jsonb storage intact, not just compared equal.
	aad := crypto.APIKeyAAD(fetched.KeyPrefix)
	if _, err := crypto.Decrypt(fod.O5LogonVerifier6949, encKey, aad); err != nil {
		t.Errorf("Decrypt(verifier_6949) after round-trip error = %v", err)
	}
	if _, err := crypto.Decrypt(fod.O5LogonVerifier18453, encKey, aad); err != nil {
		t.Errorf("Decrypt(verifier_18453) after round-trip error = %v", err)
	}

	// A key created without an encryption key has no protocol data — the jsonb
	// column is NULL and OracleData() returns nil (nullzero behavior).
	plain, _, err := store.CreateAPIKey(ctx, user.UID, "Plain Key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey(no enc) error = %v", err)
	}

	plainFetched, err := store.GetAPIKeyByPrefix(ctx, plain.KeyPrefix)
	if err != nil {
		t.Fatalf("GetAPIKeyByPrefix(plain) error = %v", err)
	}
	if plainFetched.ProtocolData != nil {
		t.Errorf("plain key ProtocolData = %+v, want nil", plainFetched.ProtocolData)
	}
	if plainFetched.OracleData() != nil {
		t.Errorf("plain key OracleData() = %+v, want nil", plainFetched.OracleData())
	}
}

func TestVerifyAPIKey(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a test user
	user, err := store.CreateUser(ctx, "verifyuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("verify valid key", func(t *testing.T) {
		_, plainKey, err := store.CreateAPIKey(ctx, user.UID, "Valid Key", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		apiKey, err := store.VerifyAPIKey(ctx, plainKey)
		if err != nil {
			t.Fatalf("VerifyAPIKey() error = %v", err)
		}

		if apiKey.UserID != user.UID {
			t.Errorf("VerifyAPIKey() apiKey.UserID = %s, want %s", apiKey.UserID, user.UID)
		}
	})

	t.Run("verify invalid key", func(t *testing.T) {
		_, err := store.VerifyAPIKey(ctx, "dbb_invalidkey12345678901234567890")
		if !errors.Is(err, ErrAPIKeyNotFound) {
			t.Errorf("VerifyAPIKey() error = %v, want %v", err, ErrAPIKeyNotFound)
		}
	})

	t.Run("verify wrong key value", func(t *testing.T) {
		created, _, err := store.CreateAPIKey(ctx, user.UID, "Wrong Key", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		// Use the correct prefix but wrong random part
		wrongKey := created.KeyPrefix + "wrongrandompart123456789012345"
		_, err = store.VerifyAPIKey(ctx, wrongKey)
		if !errors.Is(err, ErrAPIKeyNotFound) {
			t.Errorf("VerifyAPIKey() error = %v, want %v", err, ErrAPIKeyNotFound)
		}
	})

	t.Run("verify expired key", func(t *testing.T) {
		expiresAt := time.Now().Add(-1 * time.Hour) // Expired 1 hour ago
		_, plainKey, err := store.CreateAPIKey(ctx, user.UID, "Expired Key", &expiresAt)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		_, err = store.VerifyAPIKey(ctx, plainKey)
		if !errors.Is(err, ErrAPIKeyExpired) {
			t.Errorf("VerifyAPIKey() error = %v, want %v", err, ErrAPIKeyExpired)
		}
	})

	t.Run("verify revoked key", func(t *testing.T) {
		created, plainKey, err := store.CreateAPIKey(ctx, user.UID, "Revoked Key", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		// Revoke the key
		err = store.RevokeAPIKey(ctx, created.ID, user.UID)
		if err != nil {
			t.Fatalf("RevokeAPIKey() error = %v", err)
		}

		_, err = store.VerifyAPIKey(ctx, plainKey)
		if !errors.Is(err, ErrAPIKeyRevoked) {
			t.Errorf("VerifyAPIKey() error = %v, want %v", err, ErrAPIKeyRevoked)
		}
	})

	t.Run("verify key too short", func(t *testing.T) {
		_, err := store.VerifyAPIKey(ctx, "dbb_sh")
		if !errors.Is(err, ErrAPIKeyNotFound) {
			t.Errorf("VerifyAPIKey() error = %v, want %v", err, ErrAPIKeyNotFound)
		}
	})
}

func TestListAPIKeys(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create test users
	user1, err := store.CreateUser(ctx, "listuser1", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	user2, err := store.CreateUser(ctx, "listuser2", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	// Create keys for user1
	_, _, err = store.CreateAPIKey(ctx, user1.UID, "Key 1", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	_, _, err = store.CreateAPIKey(ctx, user1.UID, "Key 2", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}

	// Create key for user2
	_, _, err = store.CreateAPIKey(ctx, user2.UID, "Key 3", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}

	t.Run("list all keys", func(t *testing.T) {
		keys, err := store.ListAPIKeys(ctx, APIKeyFilter{})
		if err != nil {
			t.Fatalf("ListAPIKeys() error = %v", err)
		}
		if len(keys) != 3 {
			t.Errorf("ListAPIKeys() len = %d, want 3", len(keys))
		}
	})

	t.Run("list keys by user", func(t *testing.T) {
		keys, err := store.ListAPIKeys(ctx, APIKeyFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListAPIKeys() error = %v", err)
		}
		if len(keys) != 2 {
			t.Errorf("ListAPIKeys() len = %d, want 2", len(keys))
		}
		for _, key := range keys {
			if key.UserID != user1.UID {
				t.Errorf("ListAPIKeys() key.UserID = %s, want %s", key.UserID, user1.UID)
			}
		}
	})

	t.Run("list excludes revoked keys by default", func(t *testing.T) {
		// Revoke one of user1's keys
		keys, err := store.ListAPIKeys(ctx, APIKeyFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListAPIKeys() error = %v", err)
		}

		err = store.RevokeAPIKey(ctx, keys[0].ID, user1.UID)
		if err != nil {
			t.Fatalf("RevokeAPIKey() error = %v", err)
		}

		// List again - should only see 1 key
		keys, err = store.ListAPIKeys(ctx, APIKeyFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListAPIKeys() error = %v", err)
		}
		if len(keys) != 1 {
			t.Errorf("ListAPIKeys() len = %d, want 1", len(keys))
		}
	})

	t.Run("list includes revoked keys when IncludeAll is true", func(t *testing.T) {
		keys, err := store.ListAPIKeys(ctx, APIKeyFilter{UserID: &user1.UID, IncludeAll: true})
		if err != nil {
			t.Fatalf("ListAPIKeys() error = %v", err)
		}
		if len(keys) != 2 {
			t.Errorf("ListAPIKeys() len = %d, want 2", len(keys))
		}
	})
}

func TestGetAPIKeyByID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "getuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	created, _, err := store.CreateAPIKey(ctx, user.UID, "Get Key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}

	t.Run("existing key", func(t *testing.T) {
		apiKey, err := store.GetAPIKeyByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetAPIKeyByID() error = %v", err)
		}
		if apiKey.Name != "Get Key" {
			t.Errorf("GetAPIKeyByID() apiKey.Name = %q, want %q", apiKey.Name, "Get Key")
		}
	})

	t.Run("non-existing key", func(t *testing.T) {
		_, err := store.GetAPIKeyByID(ctx, uuid.New())
		if !errors.Is(err, ErrAPIKeyNotFound) {
			t.Errorf("GetAPIKeyByID() error = %v, want %v", err, ErrAPIKeyNotFound)
		}
	})
}

func TestRevokeAPIKey(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "revokeuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("revoke existing key", func(t *testing.T) {
		created, _, err := store.CreateAPIKey(ctx, user.UID, "Revoke Key", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		err = store.RevokeAPIKey(ctx, created.ID, user.UID)
		if err != nil {
			t.Fatalf("RevokeAPIKey() error = %v", err)
		}

		// Verify the key is revoked
		apiKey, err := store.GetAPIKeyByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetAPIKeyByID() error = %v", err)
		}
		if apiKey.RevokedAt == nil {
			t.Error("RevokeAPIKey() apiKey.RevokedAt = nil, expected a value")
		}
		if apiKey.RevokedBy == nil || *apiKey.RevokedBy != user.UID {
			t.Errorf("RevokeAPIKey() apiKey.RevokedBy = %v, want %s", apiKey.RevokedBy, user.UID)
		}
	})

	t.Run("revoke already revoked key", func(t *testing.T) {
		created, _, err := store.CreateAPIKey(ctx, user.UID, "Double Revoke", nil)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		err = store.RevokeAPIKey(ctx, created.ID, user.UID)
		if err != nil {
			t.Fatalf("RevokeAPIKey() first call error = %v", err)
		}

		// Try to revoke again
		err = store.RevokeAPIKey(ctx, created.ID, user.UID)
		if !errors.Is(err, ErrAPIKeyNotFound) {
			t.Errorf("RevokeAPIKey() second call error = %v, want %v", err, ErrAPIKeyNotFound)
		}
	})

	t.Run("revoke non-existing key", func(t *testing.T) {
		err := store.RevokeAPIKey(ctx, uuid.New(), user.UID)
		if !errors.Is(err, ErrAPIKeyNotFound) {
			t.Errorf("RevokeAPIKey() error = %v, want %v", err, ErrAPIKeyNotFound)
		}
	})
}

func TestIncrementAPIKeyUsage(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "usageuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	created, _, err := store.CreateAPIKey(ctx, user.UID, "Usage Key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}

	t.Run("increment usage", func(t *testing.T) {
		// Initial state
		apiKey, err := store.GetAPIKeyByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetAPIKeyByID() error = %v", err)
		}
		if apiKey.RequestCount != 0 {
			t.Errorf("Initial RequestCount = %d, want 0", apiKey.RequestCount)
		}
		if apiKey.LastUsedAt != nil {
			t.Errorf("Initial LastUsedAt = %v, want nil", apiKey.LastUsedAt)
		}

		// Increment
		err = store.IncrementAPIKeyUsage(ctx, created.ID)
		if err != nil {
			t.Fatalf("IncrementAPIKeyUsage() error = %v", err)
		}

		// Check updated state
		apiKey, err = store.GetAPIKeyByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetAPIKeyByID() error = %v", err)
		}
		if apiKey.RequestCount != 1 {
			t.Errorf("RequestCount = %d, want 1", apiKey.RequestCount)
		}
		if apiKey.LastUsedAt == nil {
			t.Error("LastUsedAt = nil, expected a value")
		}

		// Increment again
		err = store.IncrementAPIKeyUsage(ctx, created.ID)
		if err != nil {
			t.Fatalf("IncrementAPIKeyUsage() error = %v", err)
		}

		apiKey, err = store.GetAPIKeyByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetAPIKeyByID() error = %v", err)
		}
		if apiKey.RequestCount != 2 {
			t.Errorf("RequestCount = %d, want 2", apiKey.RequestCount)
		}
	})
}

func TestAPIKeyIsValid(t *testing.T) {
	t.Run("valid key", func(t *testing.T) {
		key := &APIKey{
			ExpiresAt: nil,
			RevokedAt: nil,
		}
		if !key.IsValid() {
			t.Error("IsValid() = false, want true")
		}
	})

	t.Run("expired key", func(t *testing.T) {
		expired := time.Now().Add(-1 * time.Hour)
		key := &APIKey{
			ExpiresAt: &expired,
			RevokedAt: nil,
		}
		if key.IsValid() {
			t.Error("IsValid() = true, want false for expired key")
		}
	})

	t.Run("revoked key", func(t *testing.T) {
		revoked := time.Now()
		key := &APIKey{
			ExpiresAt: nil,
			RevokedAt: &revoked,
		}
		if key.IsValid() {
			t.Error("IsValid() = true, want false for revoked key")
		}
	})

	t.Run("future expiration", func(t *testing.T) {
		future := time.Now().Add(1 * time.Hour)
		key := &APIKey{
			ExpiresAt: &future,
			RevokedAt: nil,
		}
		if !key.IsValid() {
			t.Error("IsValid() = false, want true for key with future expiration")
		}
	})
}

// TestCreateAPIKey_UserSharedSalts verifies the per-user O5LOGON salt scheme:
// every key created for a user derives its verifiers from the USER's shared
// salts (generated lazily on the first key), is flagged user_salt, and the
// verifier bytes equal a fresh derivation from plainKey + shared salt — so
// the Oracle proxy can keep all of a user's keys as login candidates.
func TestCreateAPIKey_UserSharedSalts(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "usersaltuser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	if user.OracleData() != nil {
		t.Fatal("new user should have no Oracle protocol data yet")
	}

	encKey := bytes.Repeat([]byte{0x42}, 32)

	keyA, plainA, err := store.CreateAPIKey(ctx, user.UID, "Key A", nil, encKey)
	if err != nil {
		t.Fatalf("CreateAPIKey(A) error = %v", err)
	}

	keyB, plainB, err := store.CreateAPIKey(ctx, user.UID, "Key B", nil, encKey)
	if err != nil {
		t.Fatalf("CreateAPIKey(B) error = %v", err)
	}

	// The user now has lazily generated shared salts.
	refreshed, err := store.GetUserByUID(ctx, user.UID)
	if err != nil {
		t.Fatalf("GetUserByUID() error = %v", err)
	}

	userData := refreshed.OracleData()
	if userData == nil {
		t.Fatal("user OracleData() = nil after key creation, want lazily generated salts")
	}
	if len(userData.O5LogonUserSalt6949) != crypto.O5LogonSaltLength {
		t.Errorf("user salt 6949 length = %d, want %d", len(userData.O5LogonUserSalt6949), crypto.O5LogonSaltLength)
	}
	if len(userData.O5LogonUserSalt18453) != crypto.O5LogonPbkdf2SaltLength {
		t.Errorf("user salt 18453 length = %d, want %d", len(userData.O5LogonUserSalt18453), crypto.O5LogonPbkdf2SaltLength)
	}

	for name, k := range map[string]*APIKey{"A": keyA, "B": keyB} {
		od := k.OracleData()
		if od == nil {
			t.Fatalf("key %s has no Oracle data", name)
		}
		if !od.UserSalt {
			t.Errorf("key %s UserSalt = false, want true", name)
		}
		if !bytes.Equal(od.O5LogonSalt6949, userData.O5LogonUserSalt6949) {
			t.Errorf("key %s salt 6949 differs from user salt", name)
		}
		if !bytes.Equal(od.O5LogonSalt18453, userData.O5LogonUserSalt18453) {
			t.Errorf("key %s salt 18453 differs from user salt", name)
		}
	}

	// The stored (encrypted) verifiers must equal a fresh derivation from the
	// plaintext key + the shared user salt.
	for name, kp := range map[string]struct {
		key   *APIKey
		plain string
	}{"A": {keyA, plainA}, "B": {keyB, plainB}} {
		aad := crypto.APIKeyAAD(kp.key.KeyPrefix)

		dec6949, err := crypto.Decrypt(kp.key.OracleData().O5LogonVerifier6949, encKey, aad)
		if err != nil {
			t.Fatalf("Decrypt(verifier 6949 %s) error = %v", name, err)
		}
		if want := crypto.DeriveO5LogonVerifierKey(kp.plain, userData.O5LogonUserSalt6949); !bytes.Equal(dec6949, want) {
			t.Errorf("key %s verifier 6949 not derived from user salt", name)
		}

		dec18453, err := crypto.Decrypt(kp.key.OracleData().O5LogonVerifier18453, encKey, aad)
		if err != nil {
			t.Fatalf("Decrypt(verifier 18453 %s) error = %v", name, err)
		}
		if want := crypto.DeriveO5LogonVerifier18453Key(kp.plain, userData.O5LogonUserSalt18453); !bytes.Equal(dec18453, want) {
			t.Errorf("key %s verifier 18453 not derived from user salt", name)
		}
	}
}

// TestEnsureUserOracleSalts_Idempotent verifies lazy generation is stable:
// repeated calls return the same salts, and salts survive a user re-read.
func TestEnsureUserOracleSalts_Idempotent(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "saltidem", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	first, err := store.EnsureUserOracleSalts(ctx, user.UID)
	if err != nil {
		t.Fatalf("EnsureUserOracleSalts() error = %v", err)
	}

	second, err := store.EnsureUserOracleSalts(ctx, user.UID)
	if err != nil {
		t.Fatalf("EnsureUserOracleSalts() second call error = %v", err)
	}

	if !bytes.Equal(first.O5LogonUserSalt6949, second.O5LogonUserSalt6949) ||
		!bytes.Equal(first.O5LogonUserSalt18453, second.O5LogonUserSalt18453) {
		t.Error("EnsureUserOracleSalts() not idempotent: salts changed between calls")
	}

	if _, err := store.EnsureUserOracleSalts(ctx, uuid.New()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("EnsureUserOracleSalts(unknown user) error = %v, want ErrUserNotFound", err)
	}
}
