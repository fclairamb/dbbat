# Password Verification Performance Optimization

## Overview

Address performance issues caused by Argon2id password verification being slow and memory-intensive. Currently, every API call and proxy connection triggers a full Argon2id hash verification using default parameters (64MB memory), causing poor performance on resource-constrained environments.

**GitHub Issue**: #22

## Problem Analysis

### Current Behavior

1. **High memory usage**: Argon2id uses 64MB memory per verification (`argon2Memory = 64 * 1024`)
2. **Repeated verification**: Every authenticated request re-verifies the password hash:
   - API middleware (`internal/api/middleware.go:122`)
   - Login endpoint (`internal/api/auth.go:83`)
   - Password change (`internal/api/auth.go:247`, `internal/api/auth.go:374`)
   - Proxy authentication (`internal/proxy/auth.go:93`)
   - API key verification (`internal/store/api_keys.go:193`)

3. **No caching**: Even the same user making rapid successive requests triggers full hash computation each time.

### Impact

- Slow API response times on modest hardware
- High memory consumption under load (64MB * concurrent requests)
- Poor user experience, especially for frontends making multiple API calls

## Goals

1. **Reduce memory footprint**: Allow configurable Argon2id parameters for environments where security/performance tradeoff is acceptable
2. **Eliminate redundant work**: Cache successful password verifications to avoid re-computing expensive hashes
3. **Maintain security**: Caching must not introduce vulnerabilities (timing attacks, stale credentials, etc.)

## Requirements

### 1. Configurable Argon2id Parameters

Allow administrators to tune Argon2id parameters via configuration.

#### 1.1 Configuration Options

| Environment Variable | Config Key | Description | Default |
|---------------------|------------|-------------|---------|
| `DBB_HASH_MEMORY_MB` | `hash.memory_mb` | Memory in MB (1-1024) | `64` |
| `DBB_HASH_TIME` | `hash.time` | Time/iterations (1-10) | `1` |
| `DBB_HASH_THREADS` | `hash.threads` | Parallelism (1-16) | `4` |

**Presets** (optional alternative to individual settings):

| Environment Variable | Config Key | Description |
|---------------------|------------|-------------|
| `DBB_HASH_PRESET` | `hash.preset` | Named preset: `default`, `low`, `minimal` |

| Preset | Memory | Time | Threads | Use Case |
|--------|--------|------|---------|----------|
| `default` | 64MB | 1 | 4 | Production (current behavior) |
| `low` | 16MB | 2 | 2 | Resource-constrained servers |
| `minimal` | 4MB | 3 | 1 | Development, testing, embedded |

**Note**: Individual settings override preset values.

#### 1.2 Configuration Struct

```go
// HashConfig holds password hashing configuration.
type HashConfig struct {
    // Preset is a named configuration preset (default, low, minimal).
    Preset string `koanf:"preset"`

    // MemoryMB is the memory parameter in megabytes (1-1024).
    MemoryMB int `koanf:"memory_mb"`

    // Time is the time/iterations parameter (1-10).
    Time int `koanf:"time"`

    // Threads is the parallelism parameter (1-16).
    Threads int `koanf:"threads"`
}
```

#### 1.3 Validation

- Memory: 1-1024 MB (reject values outside range)
- Time: 1-10 (reject values outside range)
- Threads: 1-16 (reject values outside range)
- Log warning at startup if using parameters below `low` preset

#### 1.4 Backward Compatibility

Existing password hashes store their parameters in the hash string itself:
```
$argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
```

**Verification**: Always uses parameters from the stored hash (no change needed)
**New hashes**: Use configured parameters

This means:
- Existing users can authenticate with current hashes
- New password hashes use new parameters
- Password changes update to new parameters

### 2. Password Verification Cache

Implement an in-memory cache for successful password verifications to avoid redundant hash computations.

#### 2.1 Configuration Options

| Environment Variable | Config Key | Description | Default |
|---------------------|------------|-------------|---------|
| `DBB_AUTH_CACHE_ENABLED` | `auth_cache.enabled` | Enable verification cache | `true` |
| `DBB_AUTH_CACHE_TTL` | `auth_cache.ttl` | Cache entry TTL | `5m` |
| `DBB_AUTH_CACHE_MAX_SIZE` | `auth_cache.max_size` | Maximum cache entries | `10000` |

#### 2.2 Cache Design

**Cache Key**: SHA-256 of `user_uid + ":" + password + ":" + password_hash_prefix`

Including `password_hash_prefix` (first 32 chars of stored hash) ensures cache invalidation when password changes.

**Cache Value**: Verification result (bool) + timestamp

**Eviction Policy**: LRU with TTL expiration

#### 2.3 Security Considerations

| Concern | Mitigation |
|---------|------------|
| Cache key leaks password | SHA-256 hash makes reversal impractical |
| Stale credentials after password change | Hash prefix in key auto-invalidates |
| Memory exhaustion | Max size limit with LRU eviction |
| Timing attacks | Constant-time comparison still used for cache key |

#### 2.4 Implementation

```go
// AuthCache provides caching for password verification results.
type AuthCache struct {
    cache    *lru.Cache[string, cacheEntry]
    ttl      time.Duration
    enabled  bool
    mu       sync.RWMutex
}

type cacheEntry struct {
    valid     bool
    timestamp time.Time
}

// VerifyPasswordCached verifies a password, using cache if available.
func (c *AuthCache) VerifyPasswordCached(
    userUID string,
    password string,
    storedHash string,
) (bool, error) {
    if !c.enabled {
        return crypto.VerifyPassword(storedHash, password)
    }

    cacheKey := c.computeKey(userUID, password, storedHash)

    // Check cache
    if entry, ok := c.get(cacheKey); ok {
        return entry.valid, nil
    }

    // Verify and cache result
    valid, err := crypto.VerifyPassword(storedHash, password)
    if err != nil {
        return false, err
    }

    c.set(cacheKey, valid)
    return valid, nil
}
```

#### 2.5 Cache Scope

The cache should be used for:
- API middleware authentication (Basic Auth)
- Session cookie validation (password not re-verified, but keep for consistency)
- Proxy authentication

**Not cached**:
- API key verification (separate concern, could be added later)
- Login endpoint (first verification, always compute)

### 3. API Key Verification Cache

Apply similar caching to API key verification.

#### 3.1 Design

Same pattern as password cache but with separate configuration:

| Environment Variable | Config Key | Description | Default |
|---------------------|------------|-------------|---------|
| `DBB_APIKEY_CACHE_ENABLED` | `apikey_cache.enabled` | Enable API key cache | `true` |
| `DBB_APIKEY_CACHE_TTL` | `apikey_cache.ttl` | Cache entry TTL | `5m` |

**Cache Key**: SHA-256 of `api_key_uid + ":" + plain_key + ":" + key_hash_prefix`

### 4. Metrics and Observability

Add metrics to monitor cache effectiveness.

#### 4.1 Log Messages

| Event | Level | Fields |
|-------|-------|--------|
| Cache hit | Debug | `cache=auth`, `user_uid` |
| Cache miss | Debug | `cache=auth`, `user_uid` |
| Cache parameters at startup | Info | `enabled`, `ttl`, `max_size` |
| Hash parameters at startup | Info | `memory_mb`, `time`, `threads`, `preset` |
| Low security warning | Warn | `memory_mb`, `recommended_min=16` |

## Implementation Plan

### Phase 1: Configurable Hash Parameters

1. **Config struct**: Add `HashConfig` to `config.Config`
2. **Config loading**: Parse env vars and validate
3. **Crypto package**: Make `HashPassword` accept parameters
4. **Integration**: Pass config to hash functions

### Phase 2: Password Verification Cache

1. **Cache implementation**: Create `AuthCache` in new `internal/cache/` package
2. **Integration**: Inject cache into API middleware and proxy auth
3. **Configuration**: Add cache config to `config.Config`

### Phase 3: API Key Cache

1. **Extend cache**: Add API key caching using same infrastructure
2. **Integration**: Update `store.VerifyAPIKey`

### Phase 4: Metrics

1. **Logging**: Add debug/info logging for cache operations
2. **Documentation**: Update CLAUDE.md and README

## Configuration Examples

### Development (minimal resources)

```yaml
hash:
  preset: minimal

auth_cache:
  enabled: true
  ttl: 10m
```

### Production (balanced)

```yaml
hash:
  memory_mb: 32
  time: 2
  threads: 4

auth_cache:
  enabled: true
  ttl: 5m
  max_size: 50000
```

### High-security (no caching, maximum hash strength)

```yaml
hash:
  memory_mb: 128
  time: 3
  threads: 8

auth_cache:
  enabled: false
```

## Migration Notes

- No database migrations required
- Existing password hashes remain valid (parameters stored in hash)
- New passwords use configured parameters
- Cache is purely in-memory, no persistence

## Security Considerations

1. **Hash parameter reduction**: Lower parameters reduce brute-force resistance. Document security implications clearly.

2. **Cache poisoning**: Not possible since cache only stores verification results, not credentials.

3. **Cache timing**: Cache hits are faster than misses. This is acceptable as:
   - Attacker would need to know the exact password to get a cache hit
   - Rate limiting mitigates brute-force regardless of timing

4. **Memory security**: Cache stores SHA-256 hashes of credentials, not plaintext. Memory dumps don't expose passwords.

## Testing

### Unit Tests

- Hash parameter presets resolve correctly
- Cache key generation is deterministic
- Cache eviction works (TTL, LRU)
- Password change invalidates cache entry

### Integration Tests

- API responses faster on repeated auth
- Concurrent requests don't cause race conditions
- Cache disabled mode works correctly

### Performance Tests

- Measure API latency before/after
- Memory usage under load
- Cache hit rate metrics

## Future Enhancements

- Distributed cache (Redis) for multi-instance deployments
- Adaptive hash parameters based on system resources
- Per-user cache TTL configuration
- Prometheus metrics endpoint for cache statistics
