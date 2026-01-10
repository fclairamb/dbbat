package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
