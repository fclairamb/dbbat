# Extend AuthCache to API Key and Web Session Authentication

## Overview

The AuthCache currently provides caching for password verification to avoid expensive Argon2id re-computation on every request. However, API keys and web session keys also use Argon2id hashing and currently bypass the cache entirely. This specification extends the AuthCache to cover API key and web session authentication, providing consistent performance improvements across all authentication methods.

**Related Specification**: `2026-01-11-password-verification-performance.md`

## Problem Analysis

### Current State

The `AuthCache` is used in these locations:
- **Basic Auth middleware** (`internal/api/middleware.go:123-128`)
- **PostgreSQL proxy authentication** (`internal/proxy/auth.go:94-98`)

The `AuthCache` is **NOT used** in:
- **API key verification** (`internal/store/api_keys.go:193`) - calls `crypto.VerifyPassword()` directly
- **Web session verification** - same code path as API keys
- **Login endpoint** (`internal/api/auth.go:83`) - intentionally uncached (first verification)

### API Key Verification Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ VerifyAPIKey(plainKey)                                                      │
├─────────────────────────────────────────────────────────────────────────────┤
│ 1. Extract prefix from plainKey (first 8 chars)                             │
│ 2. GetAPIKeyByPrefix() → DB lookup (fast, ~1ms)                             │
│ 3. crypto.VerifyPassword(apiKey.KeyHash, plainKey) → Argon2id (slow, 64MB)  │
│ 4. IsRevoked() → in-memory check                                            │
│ 5. IsExpired() → in-memory check                                            │
│ 6. Return apiKey                                                            │
└─────────────────────────────────────────────────────────────────────────────┘
```

**The bottleneck is step 3**: Every API request with bearer token triggers a full Argon2id verification using 64MB memory. This is identical to the password verification problem already solved by AuthCache.

### Impact

| Scenario | Without Cache | With Cache |
|----------|--------------|------------|
| API key verification | 64MB + ~100ms per request | ~0.1ms on cache hit |
| Web session validation | 64MB + ~100ms per request | ~0.1ms on cache hit |
| Concurrent API requests | Memory explosion (64MB * N) | Bounded memory |
| Frontend rapid-fire requests | Severe latency | Sub-millisecond response |

## Goals

1. **Reduce memory footprint**: Cache API key verification results to avoid 64MB allocations
2. **Improve latency**: Sub-millisecond response on cache hits
3. **Maintain security**: Revoked keys must fail immediately, expired keys must fail immediately
4. **Unified caching**: Use the same cache infrastructure for all authentication types
5. **Zero breaking changes**: Existing API and behavior remain unchanged

## Requirements

### 1. Extend AuthCache Interface

Add a new `VerifyKey` method for API key and web session verification.

```go
// VerifyKey verifies a key hash, using cache if available.
// keyID should be a unique identifier for the key (e.g., API key UUID).
// This method is suitable for API keys, web session keys, or any Argon2id-hashed credential.
func (c *AuthCache) VerifyKey(keyID, plainKey, storedHash string) (bool, error) {
    // Same implementation as VerifyPassword - reuse existing logic
    return c.verifyWithCache(keyID, plainKey, storedHash)
}
```

**Rationale**: While `VerifyPassword` would work technically, a separate `VerifyKey` method provides:
- Clearer semantics at call sites
- Separate logging/metrics if needed later
- Flexibility for different cache strategies per auth type

### 2. Modify Store.VerifyAPIKey

Inject AuthCache into the Store struct:

```go
// Store holds database connection and caching infrastructure.
type Store struct {
    db        *bun.DB
    authCache *cache.AuthCache  // Optional, may be nil
}

// NewStore creates a new store instance.
func NewStore(db *bun.DB, authCache *cache.AuthCache) *Store {
    return &Store{
        db:        db,
        authCache: authCache,
    }
}
```

Modified `VerifyAPIKey`:

```go
func (s *Store) VerifyAPIKey(ctx context.Context, plainKey string) (*APIKey, error) {
    // ... existing prefix extraction and DB lookup ...

    apiKey, err := s.GetAPIKeyByPrefix(ctx, prefix)
    if err != nil {
        return nil, err
    }

    // Use cache for hash verification
    var valid bool
    if s.authCache != nil {
        valid, err = s.authCache.VerifyKey(apiKey.ID.String(), plainKey, apiKey.KeyHash)
    } else {
        valid, err = crypto.VerifyPassword(apiKey.KeyHash, plainKey)
    }
    if err != nil || !valid {
        return nil, ErrAPIKeyNotFound
    }

    // Revocation and expiration checks remain unchanged
    if apiKey.IsRevoked() {
        return nil, ErrAPIKeyRevoked
    }

    if apiKey.IsExpired() {
        return nil, ErrAPIKeyExpired
    }

    return apiKey, nil
}
```

### 3. Cache Key Design

The cache key is a SHA-256 hash of the plaintext key only:

```
SHA-256(plainKey)
```

Where `plainKey` is the full plaintext key (e.g., `dbb_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3`).

**Rationale**:
- API keys are unique, high-entropy strings - no collision risk
- Simpler cache key computation
- The key itself is sufficient for uniqueness

**Cache invalidation** occurs when:
- Key is revoked (DB lookup still happens, revocation check rejects before cache is used)
- Key expires (DB lookup still happens, expiration check rejects)
- Cache TTL expires (5 minutes)

### 4. Security Considerations

#### 4.1 Revoked Keys

**Concern**: A revoked key might return a cached "valid" result.

**Mitigation**: The DB lookup (`GetAPIKeyByPrefix`) always happens before cache check. The `IsRevoked()` check uses fresh data from this lookup. Cache only accelerates hash verification, not authorization.

```
Request flow:
1. DB lookup (gets fresh revoked_at, expires_at)
2. Hash verification (cached)
3. IsRevoked() check (uses fresh DB data) → REJECT if revoked
4. IsExpired() check (uses fresh DB data) → REJECT if expired
```

**Result**: Revoked keys fail immediately, cache has no impact on revocation.

#### 4.2 Expired Keys

Same as revoked keys - expiration is checked against fresh DB data, not cached.

#### 4.3 Cache TTL

All authentication types use a unified 5-minute TTL:

| Auth Type | TTL | Rationale |
|-----------|-----|-----------|
| Password | 5 minutes | Session-like usage patterns |
| API Key | 5 minutes | Long-lived keys, frequent requests |
| Web Session | 5 minutes | Consistent with other auth types |

**Note**: A unified TTL simplifies configuration and is appropriate for all auth types. Revocation and expiration checks use fresh DB data regardless of cache state.

#### 4.4 Cache Isolation

Consider whether API keys and passwords should share the same cache:

| Approach | Pros | Cons |
|----------|------|------|
| Shared cache | Simpler, single config | Key collision theoretically possible (SHA-256 collision) |
| Separate caches | Better isolation, separate limits | More configuration, more memory |

**Recommendation**: Shared cache is acceptable. The cache key includes all identifying information, making collisions practically impossible (SHA-256 preimage resistance). The original spec already suggests unified caching.

### 5. Configuration

Use the existing `auth_cache` configuration for all authentication types:

```yaml
auth_cache:
  enabled: true
  ttl_seconds: 300    # 5 minutes
  max_size: 10000     # 10,000 entries
```

This single cache handles:
- Password verification (existing)
- API key verification (new)
- Web session verification (new)

No additional configuration is required. The unified cache provides consistent behavior across all authentication methods.

### 6. Metrics and Observability

#### 6.1 Logging

Update cache hit/miss logging to include auth type:

```go
slog.Debug("auth cache hit", "auth_type", "api_key", "key_id", keyID)
slog.Debug("auth cache miss", "auth_type", "api_key", "key_id", keyID)
```

#### 6.2 Stats Method Enhancement

Optionally track hits/misses per auth type:

```go
type AuthCacheStats struct {
    PasswordHits   int64
    PasswordMisses int64
    APIKeyHits     int64
    APIKeyMisses   int64
    WebKeyHits     int64
    WebKeyMisses   int64
    CurrentSize    int
}
```

**Note**: This is optional. The simple unified stats are likely sufficient for initial implementation.

## Implementation Plan

### Phase 1: Core Changes

1. **Add `VerifyKey` method to AuthCache** (`internal/cache/auth.go`)
   - Reuses same internal logic as `VerifyPassword`
   - Add optional `authType` parameter for logging differentiation

2. **Modify Store to accept AuthCache** (`internal/store/store.go`)
   - Add `authCache` field to Store struct
   - Update `NewStore` function signature

3. **Update VerifyAPIKey** (`internal/store/api_keys.go`)
   - Use `authCache.VerifyKey()` when cache is available
   - Fallback to direct verification when cache is nil

### Phase 2: Integration

4. **Update API server initialization** (`internal/api/server.go`)
   - Pass AuthCache to Store constructor

5. **Update tests** (`internal/store/api_keys_test.go`)
   - Test cached verification path
   - Test uncached fallback
   - Test revocation still works with caching
   - Test expiration still works with caching

### Phase 3: Documentation

6. **Update CLAUDE.md** with new cache scope
7. **Update original spec** to reference this extension

## Testing Requirements

### Unit Tests

| Test Case | Description |
|-----------|-------------|
| `TestVerifyAPIKey_CacheHit` | Second verification of same key hits cache |
| `TestVerifyAPIKey_CacheMiss` | First verification misses cache |
| `TestVerifyAPIKey_RevokedKeyFailsWithCache` | Revoked key fails even with valid cache entry |
| `TestVerifyAPIKey_ExpiredKeyFailsWithCache` | Expired key fails even with valid cache entry |
| `TestVerifyAPIKey_CacheDisabled` | Cache disabled falls back to direct verification |
| `TestVerifyAPIKey_DifferentKeysDifferentCacheEntries` | Two different API keys have separate cache entries |
| `TestVerifyWebSession_CacheHit` | Web session verification uses cache |

### Integration Tests

| Test Case | Description |
|-----------|-------------|
| `TestAPIEndpoint_CachedAuth` | Multiple API calls with same key show improved latency |
| `TestAPIKeyRevocation_ImmediateEffect` | Revoking key immediately blocks access |
| `TestWebSession_CachedAuth` | Web session token validation uses cache |

### Performance Tests

| Metric | Target |
|--------|--------|
| Cache hit latency | < 1ms |
| Memory per cached entry | < 100 bytes |
| 1000 concurrent API requests | No OOM (vs 64GB without cache) |

## Migration Notes

- No database migrations required
- No API changes
- No configuration changes required (uses existing `auth_cache` config)
- Backward compatible - cache is optional, code works without it

## Rollout Strategy

1. **Feature flag**: AuthCache already has `enabled` config
2. **Gradual rollout**: Monitor cache hit rate and memory usage
3. **Rollback**: Set `DBB_AUTH_CACHE_ENABLED=false` to disable

## Security Audit Checklist

- [ ] Revoked keys fail immediately (not cached)
- [ ] Expired keys fail immediately (not cached)
- [ ] Cache key doesn't leak plaintext credentials (SHA-256 hashed)
- [ ] Cache eviction prevents memory exhaustion
- [ ] TTL prevents stale entries
- [ ] No timing attacks introduced (constant-time comparison in Argon2id)

## Future Enhancements

1. **Active cache invalidation on revocation**: Explicitly remove cache entry when key is revoked
2. **Distributed cache**: Redis backend for multi-instance deployments
3. **Metrics endpoint**: Prometheus-compatible cache statistics

## Appendix: Code Diff Preview

### internal/cache/auth.go

```go
// computeKeyHash generates a cache key from plaintext key only.
// API keys are unique high-entropy strings, so no additional context needed.
func computeKeyHash(plainKey string) string {
    hash := sha256.Sum256([]byte(plainKey))
    return hex.EncodeToString(hash[:])
}

// VerifyKey verifies a key hash, using cache if available.
// keyID is used for logging only.
// This method is suitable for API keys and web session keys.
func (c *AuthCache) VerifyKey(keyID, plainKey, storedHash string) (bool, error) {
    if !c.enabled {
        return crypto.VerifyPassword(storedHash, plainKey)
    }

    cacheKey := computeKeyHash(plainKey)

    // Check cache
    c.mu.RLock()
    entry, found := c.entries[cacheKey]
    if found && time.Since(entry.timestamp) < c.ttl {
        c.mu.RUnlock()
        c.mu.Lock()
        c.hits++
        c.mu.Unlock()

        slog.Debug("auth cache hit", "auth_type", "api_key", "key_id", keyID)
        return entry.valid, nil
    }
    c.mu.RUnlock()

    // Cache miss - verify key
    c.mu.Lock()
    c.misses++
    c.mu.Unlock()

    slog.Debug("auth cache miss", "auth_type", "api_key", "key_id", keyID)

    valid, err := crypto.VerifyPassword(storedHash, plainKey)
    if err != nil {
        return false, err
    }

    c.set(cacheKey, valid)
    return valid, nil
}
```

### internal/store/api_keys.go

```go
func (s *Store) VerifyAPIKey(ctx context.Context, plainKey string) (*APIKey, error) {
    if len(plainKey) < APIKeyPrefixLength {
        return nil, ErrAPIKeyNotFound
    }

    prefix := plainKey[:APIKeyPrefixLength]

    apiKey, err := s.GetAPIKeyByPrefix(ctx, prefix)
    if err != nil {
        return nil, err
    }

    // Use cache for hash verification if available
    var valid bool
    if s.authCache != nil {
        valid, err = s.authCache.VerifyKey(apiKey.ID.String(), plainKey, apiKey.KeyHash)
    } else {
        valid, err = crypto.VerifyPassword(apiKey.KeyHash, plainKey)
    }
    if err != nil || !valid {
        return nil, ErrAPIKeyNotFound
    }

    if apiKey.IsRevoked() {
        return nil, ErrAPIKeyRevoked
    }

    if apiKey.IsExpired() {
        return nil, ErrAPIKeyExpired
    }

    return apiKey, nil
}
```
