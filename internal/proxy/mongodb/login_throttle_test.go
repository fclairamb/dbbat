package mongodb

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestVerifyCredentials_LoginThrottle: the fixed authFailDelay is no counter,
// so before the fix every PLAIN attempt reached the hash check. A listener
// built by NewServer is throttled out of the box; the auth cache is the
// hash-count hook.
func TestVerifyCredentials_LoginThrottle(t *testing.T) {
	t.Parallel()

	s, dataStore := newTeardownSession(t)
	ctx := context.Background()

	hash, err := crypto.HashPassword("right-password")
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, "throttled", hash, []string{store.RoleConnector})
	require.NoError(t, err)

	_, apiKey, err := dataStore.CreateAPIKey(ctx, user.UID, "throttle-test", nil)
	require.NoError(t, err)

	authCache := cache.NewAuthCache(cache.AuthCacheConfig{Enabled: true, TTLSeconds: 60, MaxSize: 100})
	s.server.authCache = authCache

	checks := func() int64 {
		hits, misses, _ := authCache.Stats()

		return hits + misses
	}

	got, err := s.verifyCredentials("throttled", "right-password")
	require.NoError(t, err, "a correct first attempt is never delayed")
	assert.Equal(t, user.UID, got.UID)

	for i := range 3 {
		_, err := s.verifyCredentials("throttled", fmt.Sprintf("guess-%d", i))
		require.ErrorIs(t, err, ErrAuthenticationFailed)
	}

	before := checks()

	_, err = s.verifyCredentials("throttled", "right-password")
	require.ErrorIs(t, err, ErrAuthenticationFailed, "the right password is refused while the backoff lasts")
	assert.Equal(t, before, checks(), "the throttled attempt reached the hash check")

	_, err = s.verifyCredentials("throttled", apiKey)
	require.NoError(t, err, "an API-key login is not throttled")
}
