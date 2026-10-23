package store

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGrantBytesNotifier records every event handed to it.
type fakeGrantBytesNotifier struct {
	events chan GrantBytesEvent
}

func newFakeGrantBytesNotifier() *fakeGrantBytesNotifier {
	return &fakeGrantBytesNotifier{events: make(chan GrantBytesEvent, 16)}
}

func (f *fakeGrantBytesNotifier) NotifyGrantBytes(_ context.Context, ev GrantBytesEvent) {
	f.events <- ev
}

// bytesGrantFixture is one active grant with a stamped connection to push
// bytes through.
type bytesGrantFixture struct {
	user  *User
	db    *Server
	grant *Grant
	conn  *Connection
}

func newBytesGrantFixture(t *testing.T, ctx context.Context, s *Store, notifyStep *int64, window [2]time.Time) bytesGrantFixture {
	t.Helper()

	suffix := uuid.NewString()[:8]
	user, db := createTestUserAndDatabase(t, ctx, s, "bytes"+suffix)
	admin := createTestAdmin(t, ctx, s, "bytesadmin"+suffix)

	def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{NotifyBytesStep: notifyStep})
	grant := newTestGrant(t, ctx, s, def, user.UID, db.UID, admin.UID, window[0], window[1])

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.7", WithGrantUID(grant.UID))
	require.NoError(t, err)

	return bytesGrantFixture{user: user, db: db, grant: grant, conn: conn}
}

func activeWindow() [2]time.Time {
	now := time.Now()

	return [2]time.Time{now.Add(-time.Hour), now.Add(time.Hour)}
}

func storedNotifiedSteps(t *testing.T, ctx context.Context, s *Store, uid uuid.UUID) int64 {
	t.Helper()

	g, err := s.GetGrantByUID(ctx, uid)
	require.NoError(t, err)

	return g.BytesNotifiedSteps
}

func TestSweepGrantBytes_ClaimsAStepOnce(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	notifier := newFakeGrantBytesNotifier()
	s.SetGrantBytesNotify(1000, notifier)

	f := newBytesGrantFixture(t, ctx, s, nil, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, f.conn.UID, 1500))

	events, err := s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, int64(1), events[0].Steps)
	assert.Equal(t, int64(1500), events[0].Total)
	assert.Equal(t, int64(1500), events[0].Delta)
	assert.Equal(t, f.user.UID, events[0].User.UID)
	assert.Equal(t, f.db.UID, events[0].Database.UID)
	assert.Empty(t, events[0].RequestSlackChannel, "a directly assigned grant has no request thread")

	select {
	case ev := <-notifier.events:
		assert.Equal(t, f.grant.UID, ev.Grant.UID)
	case <-time.After(5 * time.Second):
		t.Fatal("notifier was not called")
	}

	// Same bytes, second sweep: nothing new to announce.
	events, err = s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Equal(t, int64(1), storedNotifiedSteps(t, ctx, s, f.grant.UID))

	// One more step: announced, with the delta since the previous step.
	require.NoError(t, s.IncrementConnectionStats(ctx, f.conn.UID, 700))

	events, err = s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, int64(2), events[0].Steps)
	assert.Equal(t, int64(2200), events[0].Total)
	assert.Equal(t, int64(1200), events[0].Delta)

	// The audit entry survives without Slack.
	eventType := AuditEventGrantBytesThreshold
	audit, err := s.ListAuditEvents(ctx, AuditFilter{EventType: &eventType, Limit: 10})
	require.NoError(t, err)
	require.Len(t, audit, 2)

	var details grantBytesAuditDetails
	require.NoError(t, json.Unmarshal(audit[0].Details, &details))
	assert.Equal(t, f.grant.UID, details.GrantUID)
}

func TestSweepGrantBytes_ThreeStepsAnnouncedOnce(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	s.SetGrantBytesNotify(1000, nil)

	f := newBytesGrantFixture(t, ctx, s, nil, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, f.conn.UID, 3400))

	events, err := s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, int64(3), events[0].Steps)
	assert.Equal(t, int64(3400), events[0].Total)
	assert.Equal(t, int64(3), storedNotifiedSteps(t, ctx, s, f.grant.UID))
}

func TestClaimGrantBytesSteps_ConcurrentClaimsHaveOneWinner(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	f := newBytesGrantFixture(t, ctx, s, nil, activeWindow())

	const claimers = 8

	var (
		wg   sync.WaitGroup
		wins atomic.Int32
	)

	for range claimers {
		wg.Go(func() {
			ok, err := s.ClaimGrantBytesSteps(ctx, f.grant.UID, 1)
			assert.NoError(t, err)

			if ok {
				wins.Add(1)
			}
		})
	}

	wg.Wait()

	assert.Equal(t, int32(1), wins.Load())
	assert.Equal(t, int64(1), storedNotifiedSteps(t, ctx, s, f.grant.UID))

	// A lower or equal claim afterwards never wins.
	ok, err := s.ClaimGrantBytesSteps(ctx, f.grant.UID, 1)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSweepGrantBytes_SkipsRevokedAndExpiredGrants(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	s.SetGrantBytesNotify(1000, nil)

	revoked := newBytesGrantFixture(t, ctx, s, nil, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, revoked.conn.UID, 5000))
	require.NoError(t, s.RevokeGrant(ctx, revoked.grant.UID, revoked.grant.GrantedBy))

	now := time.Now()
	expired := newBytesGrantFixture(t, ctx, s, nil, [2]time.Time{now.Add(-2 * time.Hour), now.Add(-time.Minute)})
	require.NoError(t, s.IncrementConnectionStats(ctx, expired.conn.UID, 5000))

	events, err := s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Zero(t, storedNotifiedSteps(t, ctx, s, revoked.grant.UID))
	assert.Zero(t, storedNotifiedSteps(t, ctx, s, expired.grant.UID))
}

func TestSweepGrantBytes_DefinitionStepOverridesDefault(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	s.SetGrantBytesNotify(1000, nil)

	off := int64(0)
	optedOut := newBytesGrantFixture(t, ctx, s, &off, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, optedOut.conn.UID, 5000))

	small := int64(100)
	ownStep := newBytesGrantFixture(t, ctx, s, &small, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, ownStep.conn.UID, 250))

	events, err := s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1, "0 on the definition must override a positive instance default")
	assert.Equal(t, ownStep.grant.UID, events[0].Grant.UID)
	assert.Equal(t, int64(2), events[0].Steps)
	assert.Equal(t, int64(100), events[0].Step)
}

func TestSweepGrantBytes_InstanceDefaultOffIsSilent(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	f := newBytesGrantFixture(t, ctx, s, nil, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, f.conn.UID, 1<<40))

	events, err := s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestSweepGrantBytes_IdleGrantsAreNotScanned(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	s.SetGrantBytesNotify(1000, nil)

	f := newBytesGrantFixture(t, ctx, s, nil, activeWindow())
	require.NoError(t, s.IncrementConnectionStats(ctx, f.conn.UID, 5000))

	events, err := s.SweepGrantBytes(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, events, "no activity since the cutoff, nothing to check")
}

func TestSweepGrantBytes_RepliesInTheRequestThread(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	s.SetGrantBytesNotify(1000, nil)

	admin, user, db, def := setupRequestFixtures(t, ctx, s, "bytesthread")

	req, err := s.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	require.NoError(t, err)
	require.NoError(t, s.SetGrantRequestSlackMessage(ctx, req.UID, "C123", "1700000000.000100"))

	grant, _, err := s.ApproveGrantRequest(ctx, req.UID, admin.UID)
	require.NoError(t, err)

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.8", WithGrantUID(grant.UID))
	require.NoError(t, err)
	require.NoError(t, s.IncrementConnectionStats(ctx, conn.UID, 1000))

	events, err := s.SweepGrantBytes(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "C123", events[0].RequestSlackChannel)
	assert.Equal(t, "1700000000.000100", events[0].RequestSlackTS)
}
