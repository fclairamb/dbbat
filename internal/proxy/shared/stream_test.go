package shared

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestQueryEventCarriesTheHoldOutcome pins the payload a watcher needs to keep
// showing "Approved" once the released statement finishes.
//
// The completion event is the *last* thing published for a held statement, so
// if it says nothing about approval, any client folding events by query uid
// ends up with a row that has silently forgotten it was ever gated.
func TestQueryEventCarriesTheHoldOutcome(t *testing.T) {
	t.Parallel()

	st := &fakeApprovalStore{}
	reg := approval.NewRegistry()
	broker := events.New()
	connUID := uuid.New()
	user := &store.User{UID: uuid.New(), Username: "alice"}

	deps := ApprovalDeps{
		Enabled: true, Store: st, Registry: reg, Broker: broker,
		PollInterval: 10 * time.Millisecond,
	}

	gate := NewApprovalGate(deps, &store.Grant{ApprovalPatterns: []string{`(?i)DELETE`}}, connUID, user, "prod")
	publisher := NewStreamPublisher(deps, connUID, user, "prod").WithApprovals(gate)

	sub := broker.Subscribe(func(string, *events.Event) bool { return true }, 64)
	defer sub.Close()
	sub.Subscribe(events.ConnectionQueriesTopic(connUID.String()))

	done := make(chan error, 1)

	go func() {
		_, err := gate.Hold(context.Background(), HoldRequest{
			SQL: "DELETE FROM users", Pattern: `(?i)DELETE`, Guard: NewLimitGuard(nil, nil, nil),
		})
		done <- err
	}()

	pending := waitForPending(t, st)
	approver := uuid.New()

	reg.Resolve(approval.Decision{
		QueryUID: pending.UID, Status: store.ApprovalApproved,
		By: &approver, ByName: "bob", At: time.Now(),
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("approved hold returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approved hold never released")
	}

	// The statement then runs and is announced. Its in-memory record knows
	// nothing about the hold — that is exactly the gap being covered.
	publisher.Query(pending.UID, &store.Query{
		ConnectionID: connUID,
		SQLText:      "DELETE FROM users",
		ExecutedAt:   time.Now(),
	})

	ev := waitForEventType(t, sub, events.EventQuery)

	if got := ev.Data["approval_status"]; got != store.ApprovalApproved {
		t.Fatalf("completion event says approval_status=%v, want %q", got, store.ApprovalApproved)
	}

	if got := ev.Data["approval_required"]; got != true {
		t.Fatalf("completion event says approval_required=%v, want true", got)
	}

	resolver, ok := ev.Data["resolved_by"].(map[string]any)
	if !ok {
		t.Fatalf("completion event carries no resolved_by: %v", ev.Data["resolved_by"])
	}

	if resolver["display_name"] != "bob" || resolver["uid"] != approver.String() {
		t.Fatalf("completion event names the wrong approver: %v", resolver)
	}
}

// TestQueryEventWithoutAHoldCarriesNoResolver makes sure the stamping above is
// scoped to the statement that was actually held: an ordinary query on the same
// session must not inherit somebody else's approval.
func TestQueryEventWithoutAHoldCarriesNoResolver(t *testing.T) {
	t.Parallel()

	broker := events.New()
	connUID := uuid.New()
	user := &store.User{UID: uuid.New(), Username: "alice"}

	deps := ApprovalDeps{Enabled: true, Broker: broker}
	gate := NewApprovalGate(deps, &store.Grant{ApprovalPatterns: []string{`(?i)DELETE`}}, connUID, user, "prod")
	publisher := NewStreamPublisher(deps, connUID, user, "prod").WithApprovals(gate)

	// A resolution for *another* statement is remembered by the gate.
	gate.resolved.Store(&ApprovalResolution{
		QueryUID: uuid.New(), Status: store.ApprovalApproved, ByName: "bob", At: time.Now(),
	})

	sub := broker.Subscribe(func(string, *events.Event) bool { return true }, 64)
	defer sub.Close()
	sub.Subscribe(events.ConnectionQueriesTopic(connUID.String()))

	publisher.Query(uuid.New(), &store.Query{
		ConnectionID: connUID,
		SQLText:      "SELECT 1",
		ExecutedAt:   time.Now(),
	})

	ev := waitForEventType(t, sub, events.EventQuery)

	if got := ev.Data["approval_status"]; got != "" {
		t.Fatalf("an unheld query reports approval_status=%v, want empty", got)
	}

	if _, present := ev.Data["resolved_by"]; present {
		t.Fatal("an unheld query must not carry an approver")
	}
}

// TestResolutionForIgnoresOtherQueries covers the accessor's own guard.
func TestResolutionForIgnoresOtherQueries(t *testing.T) {
	t.Parallel()

	var gate *ApprovalGate
	if gate.ResolutionFor(uuid.New()) != nil {
		t.Fatal("a nil gate must resolve nothing")
	}

	gate = NewApprovalGate(ApprovalDeps{}, nil, uuid.New(), nil, "")

	if gate.ResolutionFor(uuid.New()) != nil {
		t.Fatal("a gate that resolved nothing must report nothing")
	}

	held := uuid.New()
	gate.resolved.Store(&ApprovalResolution{QueryUID: held, Status: store.ApprovalDenied})

	if gate.ResolutionFor(uuid.Nil) != nil {
		t.Fatal("uuid.Nil must never match a remembered resolution")
	}

	if r := gate.ResolutionFor(held); r == nil || r.Status != store.ApprovalDenied {
		t.Fatalf("lost the remembered resolution: %+v", r)
	}
}

func waitForEventType(t *testing.T, sub *events.Subscriber, eventType string) events.Event {
	t.Helper()

	deadline := time.After(3 * time.Second)

	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == eventType {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %q event was published", eventType)

			return events.Event{}
		}
	}
}
