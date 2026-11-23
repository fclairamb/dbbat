package shared

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// fakeLoginAuditStore records the audit events a LoginThrottle writes.
type fakeLoginAuditStore struct {
	mu     sync.Mutex
	events []*store.AuditEvent
	users  map[string]*store.User
}

func (f *fakeLoginAuditStore) LogAuditEvent(_ context.Context, event *store.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.events = append(f.events, event)

	return nil
}

var errNoSuchUser = errors.New("no such user")

func (f *fakeLoginAuditStore) GetUserByUsername(_ context.Context, username string) (*store.User, error) {
	if user, ok := f.users[username]; ok {
		return user, nil
	}

	return nil, errNoSuchUser
}

func (f *fakeLoginAuditStore) eventsOf(eventType string) []*store.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*store.AuditEvent

	for _, e := range f.events {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}

	return out
}

func TestLoginThrottle_NilThrottlesNothing(t *testing.T) {
	t.Parallel()

	var throttle *LoginThrottle

	ctx := context.Background()
	for range 20 {
		throttle.Failure(ctx, "admin", "192.0.2.1")
	}

	assert.True(t, throttle.Allow(ctx, "admin", "192.0.2.1"))
	throttle.Success(ctx, "admin", "192.0.2.1")
}

func TestLoginThrottle_BackoffPerSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	throttle := NewLoginThrottle(cache.NewAuthFailureTracker(), nil, nil, store.ProtocolPostgreSQL, true)

	require.True(t, throttle.Allow(ctx, "alice", "192.0.2.1"), "a first attempt is never delayed")

	for range 3 {
		throttle.Failure(ctx, "alice", "192.0.2.1")
	}

	assert.False(t, throttle.Allow(ctx, "alice", "192.0.2.1"), "three failures put the source in backoff")
	assert.True(t, throttle.Allow(ctx, "alice", "192.0.2.2"), "another source is not affected")
	assert.True(t, throttle.Allow(ctx, "bob", "192.0.2.1"), "another username is not affected")

	// The owner's correct password from another address clears that address
	// only: the guesser's backoff stands.
	throttle.Success(ctx, "alice", "192.0.2.2")
	assert.False(t, throttle.Allow(ctx, "alice", "192.0.2.1"))
}

func TestLoginThrottle_NotEnforcedStillCounts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tracker := cache.NewAuthFailureTracker()
	throttle := NewLoginThrottle(tracker, nil, nil, store.ProtocolMySQL, false)

	for range 5 {
		throttle.Failure(ctx, "alice", "192.0.2.1")
	}

	assert.True(t, throttle.Allow(ctx, "alice", "192.0.2.1"), "test mode never refuses")

	allowed, _ := tracker.CheckRateLimit("alice", "192.0.2.1")
	assert.False(t, allowed, "but the failures are counted in the shared tracker")
}

func TestLoginThrottle_CeilingAuditsOnceWithListener(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	user := &store.User{UID: uuid.New(), Username: "admin"}
	audit := &fakeLoginAuditStore{users: map[string]*store.User{"admin": user}}
	throttle := NewLoginThrottle(cache.NewAuthFailureTracker(), audit, nil, store.ProtocolMSSQL, true)

	for i := range cache.AuthUsernameCeiling + 10 {
		throttle.Failure(ctx, "admin", fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}

	events := audit.eventsOf(store.AuditEventAuthFailureCeiling)
	require.Len(t, events, 1, "the ceiling is audited exactly once per window")
	require.NotNil(t, events[0].UserID)
	assert.Equal(t, user.UID, *events[0].UserID)

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, "mssql", details["listener"])
	assert.Equal(t, "admin", details["username"])
	assert.EqualValues(t, cache.AuthUsernameCeiling, details["failures"])

	// The owner's correct password ends the burst, and that is audited too.
	throttle.Success(ctx, "admin", "203.0.113.9")

	successes := audit.eventsOf(store.AuditEventAuthSuccessDuringCeiling)
	require.Len(t, successes, 1)
	require.NoError(t, json.Unmarshal(successes[0].Details, &details))
	assert.Equal(t, "mssql", details["listener"])
	assert.Equal(t, "203.0.113.9", details["client_ip"])
}

func TestLoginThrottle_CeilingOnUnknownUserHasNoUserID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	audit := &fakeLoginAuditStore{}
	throttle := NewLoginThrottle(cache.NewAuthFailureTracker(), audit, nil, store.ProtocolMongoDB, true)

	for i := range cache.AuthUsernameCeiling {
		throttle.Failure(ctx, "ghost", fmt.Sprintf("10.1.%d.%d", i/256, i%256))
	}

	events := audit.eventsOf(store.AuditEventAuthFailureCeiling)
	require.Len(t, events, 1)
	assert.Nil(t, events[0].UserID)
}

func TestSourceIP(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "192.0.2.7", SourceIP(&net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 5432}))
	assert.Equal(t, "2001:db8::1", SourceIP(&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 3306}))
	assert.Empty(t, SourceIP(nil))
	assert.Equal(t, "pipe", SourceIP(&net.UnixAddr{Name: "pipe", Net: "unix"}))
}
