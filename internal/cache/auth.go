package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// AuthCache provides caching for password verification results to avoid
// expensive argon2id re-computation on every request.
type AuthCache struct {
	entries map[string]*cacheEntry
	mu      sync.RWMutex
	ttl     time.Duration
	maxSize int
	enabled bool

	// Stats for monitoring
	hits   int64
	misses int64
}

type cacheEntry struct {
	valid     bool
	timestamp time.Time
}

// AuthCacheConfig holds configuration for the auth cache.
type AuthCacheConfig struct {
	Enabled    bool
	TTLSeconds int
	MaxSize    int
}

// NewAuthCache creates a new authentication cache.
func NewAuthCache(cfg AuthCacheConfig) *AuthCache {
	cache := &AuthCache{
		entries: make(map[string]*cacheEntry),
		ttl:     time.Duration(cfg.TTLSeconds) * time.Second,
		maxSize: cfg.MaxSize,
		enabled: cfg.Enabled,
	}

	if cache.enabled {
		// Start background cleanup goroutine
		go cache.cleanupLoop()

		slog.InfoContext(context.Background(), "auth cache enabled",
			slog.Int("ttl_seconds", cfg.TTLSeconds),
			slog.Int("max_size", cfg.MaxSize))
	}

	return cache
}

// computeKey generates a cache key from user identifier, password, and hash prefix.
// Including the hash prefix ensures cache invalidation when password changes.
func computeKey(userID, password, storedHash string) string {
	// Use first 32 chars of stored hash as prefix (includes params)
	hashPrefix := storedHash
	if len(hashPrefix) > 32 {
		hashPrefix = hashPrefix[:32]
	}

	// SHA-256 of combined data for privacy
	data := userID + ":" + password + ":" + hashPrefix
	hash := sha256.Sum256([]byte(data))

	return hex.EncodeToString(hash[:])
}

// VerifyPassword verifies a password, using cache if available.
// userID should be a unique identifier for the user (e.g., UID).
func (c *AuthCache) VerifyPassword(ctx context.Context, userID, password, storedHash string) (bool, error) {
	if !c.enabled {
		return crypto.VerifyPassword(storedHash, password)
	}

	cacheKey := computeKey(userID, password, storedHash)

	// Check cache
	c.mu.RLock()
	entry, found := c.entries[cacheKey]
	if found && time.Since(entry.timestamp) < c.ttl {
		c.mu.RUnlock()
		c.mu.Lock()
		c.hits++
		c.mu.Unlock()

		slog.DebugContext(ctx, "auth cache hit", slog.String("user_id", userID))

		return entry.valid, nil
	}
	c.mu.RUnlock()

	// Cache miss - verify password
	c.mu.Lock()
	c.misses++
	c.mu.Unlock()

	slog.DebugContext(ctx, "auth cache miss", slog.String("user_id", userID))

	valid, err := crypto.VerifyPassword(storedHash, password)
	if err != nil {
		return false, err
	}

	// Store result in cache
	c.set(cacheKey, valid)

	return valid, nil
}

// set stores a verification result in the cache.
func (c *AuthCache) set(key string, valid bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict oldest entries if at capacity
	if len(c.entries) >= c.maxSize {
		c.evictOldest()
	}

	c.entries[key] = &cacheEntry{
		valid:     valid,
		timestamp: time.Now(),
	}
}

// evictOldest removes the oldest entry from the cache.
// Must be called with lock held.
func (c *AuthCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.entries {
		if oldestKey == "" || entry.timestamp.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.timestamp
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// cleanupLoop periodically removes expired entries.
func (c *AuthCache) cleanupLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()

	for range ticker.C {
		c.cleanup()
	}
}

// cleanup removes expired entries from the cache.
func (c *AuthCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.entries {
		if now.Sub(entry.timestamp) >= c.ttl {
			delete(c.entries, key)
		}
	}
}

// Stats returns cache statistics: hits, misses, and current size.
func (c *AuthCache) Stats() (int64, int64, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.hits, c.misses, len(c.entries)
}

// Clear removes all entries from the cache.
func (c *AuthCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]*cacheEntry)
}

// Enabled returns whether the cache is enabled.
func (c *AuthCache) Enabled() bool {
	return c.enabled
}
