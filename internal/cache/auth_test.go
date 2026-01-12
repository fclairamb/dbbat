package cache

import (
	"context"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/crypto"
)

func TestAuthCache_VerifyPassword(t *testing.T) {
	t.Parallel()

	// Create a test password hash
	password := "testpassword123"
	hash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	userID := "user-123"

	// First verification should miss cache
	valid, err := cache.VerifyPassword(context.Background(), userID, password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if !valid {
		t.Error("expected password to be valid")
	}

	hits, misses, _ := cache.Stats()
	if hits != 0 || misses != 1 {
		t.Errorf("expected 0 hits, 1 miss; got %d hits, %d misses", hits, misses)
	}

	// Second verification should hit cache
	valid, err = cache.VerifyPassword(context.Background(), userID, password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if !valid {
		t.Error("expected password to be valid")
	}

	hits, misses, _ = cache.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("expected 1 hit, 1 miss; got %d hits, %d misses", hits, misses)
	}
}

func TestAuthCache_WrongPassword(t *testing.T) {
	t.Parallel()

	password := "correctpassword"
	hash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	userID := "user-123"

	// Wrong password should not be valid
	valid, err := cache.VerifyPassword(context.Background(), userID, "wrongpassword", hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if valid {
		t.Error("expected wrong password to be invalid")
	}

	// Wrong password should still be cached (as invalid)
	valid, err = cache.VerifyPassword(context.Background(), userID, "wrongpassword", hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if valid {
		t.Error("expected wrong password to still be invalid")
	}

	hits, misses, _ := cache.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("expected 1 hit, 1 miss; got %d hits, %d misses", hits, misses)
	}
}

func TestAuthCache_PasswordChange(t *testing.T) {
	t.Parallel()

	oldPassword := "oldpassword"
	newPassword := "newpassword"

	oldHash, err := crypto.HashPassword(oldPassword)
	if err != nil {
		t.Fatalf("failed to hash old password: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	userID := "user-123"

	// Verify old password
	valid, err := cache.VerifyPassword(context.Background(), userID, oldPassword, oldHash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if !valid {
		t.Error("expected old password to be valid")
	}

	// Simulate password change
	newHash, err := crypto.HashPassword(newPassword)
	if err != nil {
		t.Fatalf("failed to hash new password: %v", err)
	}

	// Old password with new hash should be invalid (cache key includes hash prefix)
	valid, err = cache.VerifyPassword(context.Background(), userID, oldPassword, newHash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if valid {
		t.Error("expected old password with new hash to be invalid")
	}

	// New password with new hash should be valid
	valid, err = cache.VerifyPassword(context.Background(), userID, newPassword, newHash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if !valid {
		t.Error("expected new password to be valid")
	}
}

func TestAuthCache_Disabled(t *testing.T) {
	t.Parallel()

	password := "testpassword"
	hash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    false,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	userID := "user-123"

	// Should work without caching
	valid, err := cache.VerifyPassword(context.Background(), userID, password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if !valid {
		t.Error("expected password to be valid")
	}

	// Stats should show no hits/misses since cache is disabled
	hits, misses, size := cache.Stats()
	if hits != 0 || misses != 0 || size != 0 {
		t.Errorf("expected 0 hits, 0 misses, 0 size; got %d hits, %d misses, %d size", hits, misses, size)
	}
}

func TestAuthCache_MaxSize(t *testing.T) {
	t.Parallel()

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    3,
	})

	// Create unique passwords and hashes
	for i := 0; i < 5; i++ {
		password := "password"
		hash, _ := crypto.HashPassword(password)
		userID := string(rune('a' + i))

		_, err := cache.VerifyPassword(context.Background(), userID, password, hash)
		if err != nil {
			t.Fatalf("VerifyPassword failed: %v", err)
		}
	}

	// Should not exceed max size
	_, _, size := cache.Stats()
	if size > 3 {
		t.Errorf("cache size %d exceeds max size 3", size)
	}
}

func TestAuthCache_TTLExpiry(t *testing.T) {
	t.Parallel()

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 1, // 1 second TTL for testing
		MaxSize:    100,
	})

	password := "testpassword"
	hash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	userID := "user-123"

	// First call - cache miss
	_, err = cache.VerifyPassword(context.Background(), userID, password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}

	// Second call - cache hit
	_, err = cache.VerifyPassword(context.Background(), userID, password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}

	hits, misses, _ := cache.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("expected 1 hit, 1 miss; got %d hits, %d misses", hits, misses)
	}

	// Wait for TTL to expire
	time.Sleep(1100 * time.Millisecond)

	// Third call - cache miss (expired)
	_, err = cache.VerifyPassword(context.Background(), userID, password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}

	hits, misses, _ = cache.Stats()
	if hits != 1 || misses != 2 {
		t.Errorf("expected 1 hit, 2 misses after TTL; got %d hits, %d misses", hits, misses)
	}
}

func TestComputeKey(t *testing.T) {
	t.Parallel()

	// Same inputs should produce same key
	key1 := computeKey("user1", "password", "$argon2id$v=19$m=65536")
	key2 := computeKey("user1", "password", "$argon2id$v=19$m=65536")
	if key1 != key2 {
		t.Error("same inputs should produce same key")
	}

	// Different user should produce different key
	key3 := computeKey("user2", "password", "$argon2id$v=19$m=65536")
	if key1 == key3 {
		t.Error("different user should produce different key")
	}

	// Different password should produce different key
	key4 := computeKey("user1", "different", "$argon2id$v=19$m=65536")
	if key1 == key4 {
		t.Error("different password should produce different key")
	}

	// Different hash prefix should produce different key
	key5 := computeKey("user1", "password", "$argon2id$v=19$m=32768")
	if key1 == key5 {
		t.Error("different hash prefix should produce different key")
	}
}

func TestAuthCache_VerifyKey(t *testing.T) {
	t.Parallel()

	// Create a test API key and hash it
	apiKey := "dbb_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3"
	hash, err := crypto.HashPassword(apiKey)
	if err != nil {
		t.Fatalf("failed to hash API key: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	keyID := "key-uuid-123"

	// First verification should miss cache
	valid, err := cache.VerifyKey(context.Background(), keyID, apiKey, hash)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if !valid {
		t.Error("expected API key to be valid")
	}

	hits, misses, _ := cache.Stats()
	if hits != 0 || misses != 1 {
		t.Errorf("expected 0 hits, 1 miss; got %d hits, %d misses", hits, misses)
	}

	// Second verification should hit cache
	valid, err = cache.VerifyKey(context.Background(), keyID, apiKey, hash)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if !valid {
		t.Error("expected API key to be valid")
	}

	hits, misses, _ = cache.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("expected 1 hit, 1 miss; got %d hits, %d misses", hits, misses)
	}
}

func TestAuthCache_VerifyKey_WrongKey(t *testing.T) {
	t.Parallel()

	apiKey := "dbb_correctkey12345678901234567890"
	hash, err := crypto.HashPassword(apiKey)
	if err != nil {
		t.Fatalf("failed to hash API key: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	keyID := "key-uuid-123"

	// Wrong key should not be valid
	valid, err := cache.VerifyKey(context.Background(), keyID, "dbb_wrongkey123456789012345678901", hash)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if valid {
		t.Error("expected wrong API key to be invalid")
	}

	// Wrong key should still be cached (as invalid)
	valid, err = cache.VerifyKey(context.Background(), keyID, "dbb_wrongkey123456789012345678901", hash)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if valid {
		t.Error("expected wrong API key to still be invalid")
	}

	hits, misses, _ := cache.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("expected 1 hit, 1 miss; got %d hits, %d misses", hits, misses)
	}
}

func TestAuthCache_VerifyKey_Disabled(t *testing.T) {
	t.Parallel()

	apiKey := "dbb_testkey123456789012345678901234"
	hash, err := crypto.HashPassword(apiKey)
	if err != nil {
		t.Fatalf("failed to hash API key: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    false,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	keyID := "key-uuid-123"

	// Should work without caching
	valid, err := cache.VerifyKey(context.Background(), keyID, apiKey, hash)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if !valid {
		t.Error("expected API key to be valid")
	}

	// Stats should show no hits/misses since cache is disabled
	hits, misses, size := cache.Stats()
	if hits != 0 || misses != 0 || size != 0 {
		t.Errorf("expected 0 hits, 0 misses, 0 size; got %d hits, %d misses, %d size", hits, misses, size)
	}
}

func TestAuthCache_VerifyKey_DifferentKeys(t *testing.T) {
	t.Parallel()

	// Two different API keys
	apiKey1 := "dbb_firstkey12345678901234567890ab"
	apiKey2 := "dbb_secondkey234567890123456789abc"

	hash1, err := crypto.HashPassword(apiKey1)
	if err != nil {
		t.Fatalf("failed to hash API key 1: %v", err)
	}
	hash2, err := crypto.HashPassword(apiKey2)
	if err != nil {
		t.Fatalf("failed to hash API key 2: %v", err)
	}

	cache := NewAuthCache(AuthCacheConfig{
		Enabled:    true,
		TTLSeconds: 60,
		MaxSize:    100,
	})

	// Verify both keys
	valid, err := cache.VerifyKey(context.Background(), "key-1", apiKey1, hash1)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if !valid {
		t.Error("expected API key 1 to be valid")
	}

	valid, err = cache.VerifyKey(context.Background(), "key-2", apiKey2, hash2)
	if err != nil {
		t.Fatalf("VerifyKey failed: %v", err)
	}
	if !valid {
		t.Error("expected API key 2 to be valid")
	}

	// Both should be cache misses
	hits, misses, size := cache.Stats()
	if hits != 0 || misses != 2 || size != 2 {
		t.Errorf("expected 0 hits, 2 misses, 2 size; got %d hits, %d misses, %d size", hits, misses, size)
	}

	// Verify again - should be cache hits
	_, _ = cache.VerifyKey(context.Background(), "key-1", apiKey1, hash1)
	_, _ = cache.VerifyKey(context.Background(), "key-2", apiKey2, hash2)

	hits, misses, _ = cache.Stats()
	if hits != 2 || misses != 2 {
		t.Errorf("expected 2 hits, 2 misses; got %d hits, %d misses", hits, misses)
	}
}

func TestComputeKeyHash(t *testing.T) {
	t.Parallel()

	// Same key should produce same hash
	hash1 := computeKeyHash("dbb_testkey12345678901234567890ab")
	hash2 := computeKeyHash("dbb_testkey12345678901234567890ab")
	if hash1 != hash2 {
		t.Error("same key should produce same hash")
	}

	// Different keys should produce different hashes
	hash3 := computeKeyHash("dbb_otherkey12345678901234567890ab")
	if hash1 == hash3 {
		t.Error("different keys should produce different hashes")
	}

	// Hash should be deterministic
	if len(hash1) != 64 { // SHA-256 produces 64 hex chars
		t.Errorf("expected hash length of 64, got %d", len(hash1))
	}
}
