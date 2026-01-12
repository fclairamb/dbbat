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
