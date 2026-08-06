package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestLimitGuard_SessionDiesWithItsGrant is the regression guard for the
// invariant that multi-grant selection must not weaken: priority selection
// happens exactly once, at authentication, and the session stays pinned to the
// grant it was admitted under for its whole life.
//
// The scenario: a user holds two overlapping active grants on one database.
// Grant A (full write, priority 100) wins selection but expires soon; grant B
// (read-only, priority 10) outlives it. When A expires the session must be
// torn down — there is no mid-session failover to B, because failing over
// would silently change the session's controls underneath a live connection
// (a write session quietly continuing under a read-only grant, or vice versa).
// The client reconnects and selection runs afresh; that half is covered by
// TestGetActiveGrant_ReselectsAfterTheWinnerExpires in internal/store.
func TestLimitGuard_SessionDiesWithItsGrant(t *testing.T) {
	t.Parallel()

	now := time.Now()

	admitted := &store.Grant{
		// Full write, so it is the one priority selection hands to the proxy.
		Priority:  store.PriorityFullWrite,
		ExpiresAt: now.Add(30 * time.Second),
	}

	// Still active, still selectable — and completely irrelevant to a session
	// already admitted under `admitted`.
	other := &store.Grant{
		Controls:  []string{store.ControlReadOnly},
		Priority:  store.PriorityReadOnly,
		ExpiresAt: now.Add(2 * time.Hour),
	}

	guard := NewLimitGuard(admitted, &atomic.Int64{}, &atomic.Int64{})

	// Fast-forward past the admitted grant's expiry while the other grant is
	// still comfortably in its window.
	after := now.Add(time.Minute)
	guard.setNow(func() time.Time { return after })

	if !after.Before(other.ExpiresAt) {
		t.Fatalf("test setup is wrong: the surviving grant must still be active at %v", after)
	}

	if err := guard.Check(); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("Check() = %v, want ErrGrantExpired — the session must die with the grant it was "+
			"admitted under, even though another active grant would still allow access", err)
	}

	// The watchdog must reach the same verdict, since it is what actually
	// force-closes the connection in all four proxies.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	violation := make(chan error, 1)
	go guard.Watch(ctx, 10*time.Millisecond, func(err error) { violation <- err })

	select {
	case err := <-violation:
		if !errors.Is(err, ErrGrantExpired) {
			t.Fatalf("Watch() reported %v, want ErrGrantExpired", err)
		}
	case <-ctx.Done():
		t.Fatal("Watch() never fired for the expired grant")
	}

	// A guard built from the surviving grant does not trip — proving the
	// teardown above is about the pinned grant specifically, not about the
	// clock having moved for everyone.
	survivor := NewLimitGuard(other, &atomic.Int64{}, &atomic.Int64{})
	survivor.setNow(func() time.Time { return after })

	if err := survivor.Check(); err != nil {
		t.Fatalf("the surviving grant's guard tripped with %v, want nil", err)
	}
}
