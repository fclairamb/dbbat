package postgresql

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// throttledHandshake is leakFixture.handshake with a login throttle and an
// auth cache installed. The cache doubles as the hash-count hook: every
// password check goes through it and bumps its hits or misses, so an attempt
// refused before the check leaves both unchanged.
func (f *leakFixture) throttledHandshake(
	t *testing.T, throttle *shared.LoginThrottle, authCache *cache.AuthCache, password string,
) (*Session, error) {
	t.Helper()

	startup := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": leakUsername + shared.UsernameServerSeparator + leakEntryName, "database": leakUpstreamDB},
	}

	script, err := startup.Encode(nil)
	require.NoError(t, err)

	script, err = (&pgproto3.PasswordMessage{Password: password}).Encode(script)
	require.NoError(t, err)

	session := &Session{
		ctx:           context.Background(),
		store:         f.store,
		logger:        slog.Default(),
		clientConn:    &captureConn{},
		clientReader:  bufio.NewReader(bytes.NewReader(script)),
		authCache:     authCache,
		loginThrottle: throttle,
	}

	return session, session.authenticate()
}

func newCountingAuthCache() *cache.AuthCache {
	return cache.NewAuthCache(cache.AuthCacheConfig{Enabled: true, TTLSeconds: 60, MaxSize: 100})
}

// passwordChecks is how many password checks the cache has seen.
func passwordChecks(c *cache.AuthCache) int64 {
	hits, misses, _ := c.Stats()

	return hits + misses
}

// TestAuthenticate_LoginThrottle is the regression test for the proxy
// listener being an unthrottled password oracle: before the fix every attempt
// reached VerifyPassword, however many had failed before it.
func TestAuthenticate_LoginThrottle(t *testing.T) {
	t.Parallel()

	f := newLeakFixture(t)

	t.Run("a source in backoff is refused before any hash check", func(t *testing.T) {
		t.Parallel()

		throttle := shared.NewLoginThrottle(cache.NewAuthFailureTracker(), f.store, nil, store.ProtocolPostgreSQL, true)
		authCache := newCountingAuthCache()

		for i := range 3 {
			_, err := f.throttledHandshake(t, throttle, authCache, fmt.Sprintf("guess-%d", i))
			require.ErrorIs(t, err, ErrInvalidPassword)
		}

		require.EqualValues(t, 3, passwordChecks(authCache), "every wrong password was checked")

		// The right password, while the backoff lasts: refused, and not checked.
		session, err := f.throttledHandshake(t, throttle, authCache, leakPassword)
		require.ErrorIs(t, err, ErrInvalidPassword)
		assert.False(t, session.authenticated)
		assert.EqualValues(t, 3, passwordChecks(authCache), "the throttled attempt reached the hash check")
	})

	t.Run("a correct first attempt is never delayed", func(t *testing.T) {
		t.Parallel()

		throttle := shared.NewLoginThrottle(cache.NewAuthFailureTracker(), f.store, nil, store.ProtocolPostgreSQL, true)

		session, err := f.throttledHandshake(t, throttle, newCountingAuthCache(), leakPassword)
		require.NoError(t, err)
		assert.True(t, session.authenticated)
	})

	t.Run("an API-key login is not throttled", func(t *testing.T) {
		t.Parallel()

		user, err := f.store.GetUserByUsername(context.Background(), leakUsername)
		require.NoError(t, err)

		_, apiKey, err := f.store.CreateAPIKey(context.Background(), user.UID, "throttle-test", nil)
		require.NoError(t, err)

		throttle := shared.NewLoginThrottle(cache.NewAuthFailureTracker(), f.store, nil, store.ProtocolPostgreSQL, true)
		for range 5 {
			throttle.Failure(context.Background(), leakUsername, "10.0.0.1")
		}

		session, err := f.throttledHandshake(t, throttle, newCountingAuthCache(), apiKey)
		require.NoError(t, err)
		assert.True(t, session.authenticated)
	})
}

// Reaching the per-username ceiling through the PostgreSQL listener writes
// exactly one auth.failure_ceiling entry, tagged with the listener.
func TestAuthenticate_LoginThrottleCeilingAudit(t *testing.T) {
	t.Parallel()

	f := newLeakFixture(t)
	tracker := cache.NewAuthFailureTracker()
	throttle := shared.NewLoginThrottle(tracker, f.store, nil, store.ProtocolPostgreSQL, true)

	// Spread over other addresses, as a distributed guess would be.
	for i := range cache.AuthUsernameCeiling - 1 {
		tracker.RecordFailure(leakUsername, fmt.Sprintf("10.7.%d.%d", i/256, i%256))
	}

	for range 2 {
		_, err := f.throttledHandshake(t, throttle, nil, "wrong")
		require.ErrorIs(t, err, ErrInvalidPassword)
	}

	eventType := store.AuditEventAuthFailureCeiling
	events, err := f.store.ListAuditEvents(context.Background(), store.AuditFilter{EventType: &eventType})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UserID)

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, "postgresql", details["listener"])
	assert.Equal(t, leakUsername, details["username"])
	assert.Equal(t, "10.0.0.1", details["last_client_ip"])
}
