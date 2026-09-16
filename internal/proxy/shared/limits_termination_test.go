package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

func TestLimitGuard_Check_AdminTermination(t *testing.T) {
	t.Parallel()

	registry := cache.NewSessionRegistry()
	conn := uuid.New()
	handle := registry.Register(conn)

	grant := &store.Grant{
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{}).WithTermination(handle)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() before terminate = %v, want nil", err)
	}

	registry.Terminate(conn, cache.TerminationRequest{
		Reason: store.TerminationAdminTerminated,
		By:     "alice",
	})

	if err := g.Check(); !errors.Is(err, ErrAdminTerminated) {
		t.Fatalf("Check() after terminate = %v, want ErrAdminTerminated", err)
	}
}

func TestLimitGuard_Check_AdminTerminationTakesPrecedence(t *testing.T) {
	t.Parallel()

	registry := cache.NewSessionRegistry()
	conn := uuid.New()
	handle := registry.Register(conn)

	registry.Terminate(conn, cache.TerminationRequest{Reason: store.TerminationAdminTerminated})

	var revoked atomic.Bool

	revoked.Store(true)

	// Expired, revoked, *and* terminated. A human explicitly ending this one
	// session is the most specific instruction the guard has been given, so it
	// is the reason reported.
	grant := &store.Grant{
		ExpiresAt:  time.Now().Add(-time.Hour),
		Definition: &store.GrantDefinition{},
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{}).
		WithRevocation(&revoked).
		WithTermination(handle)

	if err := g.Check(); !errors.Is(err, ErrAdminTerminated) {
		t.Fatalf("Check() = %v, want ErrAdminTerminated to take precedence", err)
	}
}

func TestLimitGuard_Watch_KeepsRunningForTerminationWithoutLimits(t *testing.T) {
	t.Parallel()

	// A guard with no grant at all, but a termination handle attached: Watch
	// must keep polling, or a terminate would never reach a session whose grant
	// carries no limits.
	registry := cache.NewSessionRegistry()
	conn := uuid.New()
	handle := registry.Register(conn)

	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{}).WithTermination(handle)

	got := make(chan error, 1)

	go g.Watch(context.Background(), 5*time.Millisecond, func(err error) {
		got <- err
	})

	time.Sleep(10 * time.Millisecond)
	registry.Terminate(conn, cache.TerminationRequest{Reason: store.TerminationAdminTerminated})

	select {
	case err := <-got:
		if !errors.Is(err, ErrAdminTerminated) {
			t.Fatalf("Watch onViolation = %v, want ErrAdminTerminated", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not fire onViolation for a termination-only guard")
	}
}

// TestTerminationFor_AdminTerminated verifies the record carries the human, so
// the audit entry and the in-flight statement's row name a person rather than
// "dbbat".
func TestTerminationFor_AdminTerminated(t *testing.T) {
	t.Parallel()

	registry := cache.NewSessionRegistry()
	conn := uuid.New()
	handle := registry.Register(conn)

	registry.Terminate(conn, cache.TerminationRequest{
		Reason: store.TerminationAdminTerminated,
		By:     "alice",
		Detail: "runaway report",
	})

	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{}).WithTermination(handle)

	queryUID := uuid.New()

	got := TerminationFor(ErrAdminTerminated, g, queryUID)
	if got.Reason != store.TerminationAdminTerminated {
		t.Errorf("Reason = %q, want %q", got.Reason, store.TerminationAdminTerminated)
	}

	if got.By != "alice" {
		t.Errorf("By = %q, want alice", got.By)
	}

	if got.Detail != "runaway report" {
		t.Errorf("Detail = %q, want %q", got.Detail, "runaway report")
	}

	if got.QueryUID != queryUID {
		t.Errorf("QueryUID = %v, want %v", got.QueryUID, queryUID)
	}

	if want := "session terminated by alice: runaway report"; got.Message() != want {
		t.Errorf("Message() = %q, want %q", got.Message(), want)
	}
}

// TestTerminationFor_CrossInstanceRevocationKeepsItsReason is the subtle one:
// the poller relays a grant revoked on another replica through the *same* flag
// as an admin terminate, and the record must still say grant_revoked. Reading
// the sentinel instead of the handle would misreport every cross-replica
// revocation as an admin killing a session.
func TestTerminationFor_CrossInstanceRevocationKeepsItsReason(t *testing.T) {
	t.Parallel()

	registry := cache.NewSessionRegistry()
	conn := uuid.New()
	handle := registry.Register(conn)

	registry.Terminate(conn, cache.TerminationRequest{Reason: store.TerminationGrantRevoked})

	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{}).WithTermination(handle)

	got := TerminationFor(ErrAdminTerminated, g, uuid.Nil)
	if got.Reason != store.TerminationGrantRevoked {
		t.Fatalf("Reason = %q, want %q", got.Reason, store.TerminationGrantRevoked)
	}

	if got.By != "" {
		t.Errorf("By = %q, want empty: nobody asked for this session specifically", got.By)
	}
}

// TestTerminationReasonFor_AdminTerminated pins the default reason for the
// sentinel, which is what a session with no handle attached would record.
func TestTerminationReasonFor_AdminTerminated(t *testing.T) {
	t.Parallel()

	if got := TerminationReasonFor(ErrAdminTerminated); got != store.TerminationAdminTerminated {
		t.Fatalf("TerminationReasonFor(ErrAdminTerminated) = %q, want %q",
			got, store.TerminationAdminTerminated)
	}
}
