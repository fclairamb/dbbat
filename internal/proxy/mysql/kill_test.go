package mysql

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// killFixture registers a target session of user B, parked on an approval
// hold, under connection id 42.
func killFixture(t *testing.T) (*Server, *store.User, <-chan approval.Decision) {
	t.Helper()

	registry := approval.NewRegistry()
	srv := &Server{
		sessions:     newSessionRegistry(),
		approvalDeps: shared.ApprovalDeps{Enabled: true, Registry: registry},
		logger:       discardLogger(),
		ctx:          context.Background(),
	}

	userB := &store.User{UID: uuid.New(), Username: "bob"}
	target := &Session{server: srv, user: userB, logger: discardLogger(), ctx: context.Background()}

	held := uuid.New()
	decisions, cancel := registry.Register(held)
	t.Cleanup(cancel)
	target.setHeldQuery(held)
	srv.sessions.register(42, target)

	return srv, userB, decisions
}

func TestReleaseHoldForKill_OtherUserLeavesHoldPending(t *testing.T) {
	t.Parallel()

	srv, _, decisions := killFixture(t)

	userA := &store.User{UID: uuid.New(), Username: "alice"}
	caller := &Session{server: srv, user: userA, logger: discardLogger(), ctx: context.Background()}

	if caller.releaseHoldForKill("KILL QUERY 42") {
		t.Fatal("another user's KILL released the hold")
	}

	select {
	case d := <-decisions:
		t.Fatalf("hold resolved by another user: %+v", d)
	default:
	}

	if got := len(srv.approvalDeps.Registry.Pending()); got != 1 {
		t.Fatalf("pending holds = %d, want 1", got)
	}
}

func TestReleaseHoldForKill_NoUserLeavesHoldPending(t *testing.T) {
	t.Parallel()

	srv, _, _ := killFixture(t)
	caller := &Session{server: srv, logger: discardLogger(), ctx: context.Background()}

	if caller.releaseHoldForKill("KILL 42") {
		t.Fatal("a session without a user released the hold")
	}
}

func TestReleaseHoldForKill_SameUserReleasesHold(t *testing.T) {
	t.Parallel()

	srv, userB, decisions := killFixture(t)

	// A second session of the same user, distinct *store.User value.
	sameB := &store.User{UID: userB.UID, Username: userB.Username}
	caller := &Session{server: srv, user: sameB, logger: discardLogger(), ctx: context.Background()}

	if !caller.releaseHoldForKill("KILL QUERY 42") {
		t.Fatal("the owner's KILL did not release the hold")
	}

	select {
	case d := <-decisions:
		if d.Status != store.ApprovalAbandoned {
			t.Fatalf("status = %q, want %q", d.Status, store.ApprovalAbandoned)
		}
	default:
		t.Fatal("no decision delivered")
	}
}
