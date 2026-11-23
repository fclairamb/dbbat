package mssql

import (
	"context"
	"fmt"
	"testing"

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
	dataStore := newTestStore(t)

	hash, err := crypto.HashPassword("right-password")
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, "throttled", hash, []string{store.RoleConnector})
	require.NoError(t, err)

	_, apiKey, err := dataStore.CreateAPIKey(ctx, user.UID, "throttle-test", nil)
	require.NoError(t, err)

	newSession := func() (*session, *cache.AuthCache) {
		authCache := cache.NewAuthCache(cache.AuthCacheConfig{Enabled: true, TTLSeconds: 60, MaxSize: 100})
		throttle := shared.NewLoginThrottle(cache.NewAuthFailureTracker(), dataStore, nil, store.ProtocolMSSQL, true)

		return &session{server: &Server{
			store: dataStore, authCache: authCache, loginThrottle: throttle,
		}}, authCache
	}

	checks := func(c *cache.AuthCache) int64 {
		hits, misses, _ := c.Stats()

		return hits + misses
	}

	t.Run("a source in backoff is refused before any hash check", func(t *testing.T) {
		t.Parallel()

		s, authCache := newSession()

		for i := range 3 {
			_, err := s.verifyCredentials(ctx, "throttled", fmt.Sprintf("guess-%d", i), "192.0.2.1")
			require.ErrorIs(t, err, ErrAuthFailed)
		}

		require.EqualValues(t, 3, checks(authCache))

		_, err := s.verifyCredentials(ctx, "throttled", "right-password", "192.0.2.1")
		require.ErrorIs(t, err, ErrAuthFailed, "the right password is refused while the backoff lasts")
		assert.EqualValues(t, 3, checks(authCache), "the throttled attempt reached the hash check")

		// Another source is not affected.
		_, err = s.verifyCredentials(ctx, "throttled", "right-password", "192.0.2.2")
		require.NoError(t, err)

		// Nor is an API key, from the throttled source.
		_, err = s.verifyCredentials(ctx, "throttled", apiKey, "192.0.2.1")
		require.NoError(t, err)
	})

	t.Run("a correct first attempt is never delayed", func(t *testing.T) {
		t.Parallel()

		s, _ := newSession()
		_, err := s.verifyCredentials(ctx, "throttled", "right-password", "192.0.2.1")
		require.NoError(t, err)
	})
}
