package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeTerminationNotifier records every event it is handed. When block is
// non-nil, NotifyTermination parks on it until the test closes it — the seam
// TestNotifyTermination_DoesNotDelayTheCaller uses to prove the call is truly
// fire-and-forget.
type fakeTerminationNotifier struct {
	mu     sync.Mutex
	events []TerminationEvent
	block  chan struct{}
}

func (f *fakeTerminationNotifier) NotifyTermination(_ context.Context, ev TerminationEvent) {
	if f.block != nil {
		<-f.block
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.events = append(f.events, ev)
}

func (f *fakeTerminationNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.events)
}

func (f *fakeTerminationNotifier) first() TerminationEvent {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.events[0]
}

// TestNotifyTermination_ExcludedReasonsNeverFire pins the spec's core
// behavioral rule: grant_revoked, grant_expired and instance_lost never reach
// the notifier, however it is configured. The check happens before any
// goroutine is spawned, so there is nothing to wait for — an immediate
// zero-count read after CloseConnectionWithReason returns is conclusive.
func TestNotifyTermination_ExcludedReasonsNeverFire(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	notifier := &fakeTerminationNotifier{}
	s.SetTerminationNotifier(notifier)

	user, db := createTestUserAndDatabase(t, ctx, s, "excluded")

	for _, reason := range []string{TerminationGrantRevoked, TerminationGrantExpired, TerminationInstanceLost} {
		conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.9")
		require.NoError(t, err)

		require.NoError(t, s.CloseConnectionWithReason(ctx, conn.UID, Termination{Reason: reason}))
	}

	require.Equal(t, 0, notifier.count(), "none of the three excluded reasons should reach the notifier")
}

// TestNotifyTermination_IncludedReasonsFire is the positive counterpart: the
// three reasons the spec says are worth a human's attention do reach the
// notifier, with the event carrying the resolved user/database.
func TestNotifyTermination_IncludedReasonsFire(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	notifier := &fakeTerminationNotifier{}
	s.SetTerminationNotifier(notifier)

	user, db := createTestUserAndDatabase(t, ctx, s, "included")

	for _, reason := range []string{TerminationStatementTimeout, TerminationAdminTerminated, TerminationQuotaExceeded} {
		conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.10")
		require.NoError(t, err)

		require.NoError(t, s.CloseConnectionWithReason(ctx, conn.UID, Termination{Reason: reason}))
	}

	require.Eventually(t, func() bool {
		return notifier.count() == 3
	}, 2*time.Second, 10*time.Millisecond, "all three included reasons should reach the notifier")
}

// TestNotifyTermination_NilNotifierTolerated is the store-side half of "the
// producer tolerates a nil notifier": a store that never had
// SetTerminationNotifier called must close a dbbat-initiated termination
// exactly as if the notifier were configured, just without notifying.
func TestNotifyTermination_NilNotifierTolerated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "nilnotifier")

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.11")
	require.NoError(t, err)

	require.NoError(t, s.CloseConnectionWithReason(ctx, conn.UID, Termination{
		Reason: TerminationStatementTimeout,
		Limit:  30 * time.Second,
	}))
}

// TestNotifyTermination_DoesNotDelayTheCaller proves the fire-and-forget
// contract: a notifier that blocks indefinitely must not delay
// CloseConnectionWithReason's return. The event still lands once the
// notifier is unblocked, which is what tells the two apart from "never
// called".
func TestNotifyTermination_DoesNotDelayTheCaller(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	block := make(chan struct{})
	notifier := &fakeTerminationNotifier{block: block}
	s.SetTerminationNotifier(notifier)

	user, db := createTestUserAndDatabase(t, ctx, s, "blocking")

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.12")
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, s.CloseConnectionWithReason(ctx, conn.UID, Termination{
		Reason: TerminationStatementTimeout,
		Limit:  30 * time.Second,
	}))
	elapsed := time.Since(start)

	require.Less(t, elapsed, 500*time.Millisecond, "the close must return long before the blocked notifier ever would")
	require.Equal(t, 0, notifier.count(), "the notifier is still parked, so nothing has landed yet")

	close(block)

	require.Eventually(t, func() bool {
		return notifier.count() == 1
	}, 2*time.Second, 10*time.Millisecond, "unblocking the notifier lets the parked call complete")

	ev := notifier.first()
	require.Equal(t, conn.UID, ev.Connection.UID)
	require.NotNil(t, ev.User)
	require.Equal(t, user.UID, ev.User.UID)
	require.NotNil(t, ev.Database)
	require.Equal(t, db.UID, ev.Database.UID)
}
