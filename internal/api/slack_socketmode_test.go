package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/fclairamb/dbbat/internal/notify"
)

// blockActionCallback builds a block_actions interaction callback carrying one
// of our Approve/Deny buttons, as Socket Mode delivers it (already parsed).
func blockActionCallback(actionID, value, responseURL string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:        slack.InteractionTypeBlockActions,
		User:        slack.User{ID: "U1"},
		ResponseURL: responseURL,
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionID, Value: value},
			},
		},
	}
}

func TestSocketInteractionCallback_Interactive(t *testing.T) {
	t.Parallel()

	cb := blockActionCallback(notify.ActionApprove, uuid.New().String(), "")

	got, ok := socketInteractionCallback(socketmode.Event{Type: socketmode.EventTypeInteractive, Data: cb})
	if !ok {
		t.Fatal("expected ok for an interactive event")
	}

	if got.User.ID != "U1" {
		t.Fatalf("callback not returned intact: %+v", got)
	}
}

func TestSocketInteractionCallback_NonInteractive(t *testing.T) {
	t.Parallel()

	if _, ok := socketInteractionCallback(socketmode.Event{Type: socketmode.EventTypeConnected}); ok {
		t.Fatal("expected a non-interactive event to be ignored")
	}
}

func TestSocketInteractionCallback_WrongData(t *testing.T) {
	t.Parallel()

	evt := socketmode.Event{Type: socketmode.EventTypeInteractive, Data: "not a callback"}
	if _, ok := socketInteractionCallback(evt); ok {
		t.Fatal("expected an unexpected data payload to be ignored")
	}
}

func TestDispatchSlackCallback_AdminApprove(t *testing.T) {
	t.Parallel()

	srv := testServer()
	stub := newStubDecider()
	stub.user = adminUser()
	stub.approveOutcome = sampleOutcome(notify.GrantActionApproved)

	cb := blockActionCallback(notify.ActionApprove, uuid.New().String(), "")
	if !srv.dispatchSlackCallback(stub, cb) {
		t.Fatal("expected dispatch to accept an approve callback")
	}

	stub.waitDone(t)

	if !stub.approveCalled {
		t.Error("expected approve to be called")
	}

	if !stub.threadReplied {
		t.Error("expected a thread reply on success")
	}

	if stub.lastDeciderUID != stub.user.UID {
		t.Error("approve called with the wrong decider")
	}
}

func TestDispatchSlackCallback_AdminDeny(t *testing.T) {
	t.Parallel()

	srv := testServer()
	stub := newStubDecider()
	stub.user = adminUser()
	stub.denyOutcome = sampleOutcome(notify.GrantActionDenied)

	cb := blockActionCallback(notify.ActionDeny, uuid.New().String(), "")
	if !srv.dispatchSlackCallback(stub, cb) {
		t.Fatal("expected dispatch to accept a deny callback")
	}

	stub.waitDone(t)

	if !stub.denyCalled {
		t.Error("expected deny to be called")
	}

	if stub.approveCalled {
		t.Error("approve must not be called for a deny")
	}
}

func TestDispatchSlackCallback_NonAdminEphemeral(t *testing.T) {
	t.Parallel()

	srv := testServer()
	rec := newEphemeralRecorder(t)
	stub := newStubDecider()
	stub.user = nonAdminUser()

	cb := blockActionCallback(notify.ActionApprove, uuid.New().String(), rec.srv.URL)
	if !srv.dispatchSlackCallback(stub, cb) {
		t.Fatal("expected dispatch to accept the callback")
	}

	rec.wait(t)

	if stub.approveCalled {
		t.Error("a non-admin click must not trigger approve")
	}

	if got := rec.lastPost(); got != msgNotAdmin {
		t.Errorf("expected the non-admin ephemeral, got %q", got)
	}
}

func TestDispatchSlackCallback_UnknownActionNoOp(t *testing.T) {
	t.Parallel()

	srv := testServer()
	stub := newStubDecider()
	stub.user = adminUser()

	cb := blockActionCallback("some_other_action", uuid.New().String(), "")
	if srv.dispatchSlackCallback(stub, cb) {
		t.Fatal("expected dispatch to reject an unknown action")
	}

	if stub.approveCalled || stub.denyCalled {
		t.Error("no decision should run for an unknown action")
	}
}
