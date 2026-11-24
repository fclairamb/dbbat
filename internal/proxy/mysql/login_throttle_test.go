package mysql

import (
	"context"
	"fmt"
	"testing"

	gomysqlserver "github.com/go-mysql-org/go-mysql/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestVerifyCredentials_LoginThrottle: before the fix every attempt reached
// the hash check, however many had failed before it from the same source.
// The auth cache is the hash-count hook: each check bumps its hits or misses.
func TestVerifyCredentials_LoginThrottle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataStore := newTeardownTestStore(t)

	hash, err := crypto.HashPassword("right-password")
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, "throttled", hash, []string{store.RoleConnector})
	require.NoError(t, err)

	_, apiKey, err := dataStore.CreateAPIKey(ctx, user.UID, "throttle-test", nil)
	require.NoError(t, err)

	newProvider := func() (*dbbatAuthProvider, *cache.AuthCache) {
		authCache := cache.NewAuthCache(cache.AuthCacheConfig{Enabled: true, TTLSeconds: 60, MaxSize: 100})
		throttle := shared.NewLoginThrottle(cache.NewAuthFailureTracker(), dataStore, nil, store.ProtocolMySQL, true)

		return &dbbatAuthProvider{server: &Server{
			store: dataStore, ctx: ctx, authCache: authCache, loginThrottle: throttle,
		}}, authCache
	}

	checks := func(c *cache.AuthCache) int64 {
		hits, misses, _ := c.Stats()

		return hits + misses
	}

	t.Run("a source in backoff is refused before any hash check", func(t *testing.T) {
		t.Parallel()

		provider, authCache := newProvider()

		for i := range 3 {
			err := provider.verifyCredentials("throttled", fmt.Sprintf("guess-%d", i), "192.0.2.1")
			require.ErrorIs(t, err, gomysqlserver.ErrAccessDenied)
		}

		require.EqualValues(t, 3, checks(authCache))

		err := provider.verifyCredentials("throttled", "right-password", "192.0.2.1")
		require.ErrorIs(t, err, gomysqlserver.ErrAccessDenied, "the right password is refused while the backoff lasts")
		assert.EqualValues(t, 3, checks(authCache), "the throttled attempt reached the hash check")

		// Another source is not affected.
		require.NoError(t, provider.verifyCredentials("throttled", "right-password", "192.0.2.2"))

		// Nor is an API key, from the throttled source.
		require.NoError(t, provider.verifyCredentials("throttled", apiKey, "192.0.2.1"))
	})

	t.Run("a correct first attempt is never delayed", func(t *testing.T) {
		t.Parallel()

		provider, _ := newProvider()
		require.NoError(t, provider.verifyCredentials("throttled", "right-password", "192.0.2.1"))
	})

	t.Run("unknown users count as failures", func(t *testing.T) {
		t.Parallel()

		provider, _ := newProvider()

		for range 3 {
			require.ErrorIs(t, provider.verifyCredentials("nobody", "x", "192.0.2.1"), gomysqlserver.ErrAccessDenied)
		}

		assert.False(t, provider.server.loginThrottle.Allow(ctx, "nobody", "192.0.2.1"))
	})
}
