package postgresql

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// A FunctionCall names its function by OID and carries no statement text, so
// it is refused whatever the grant says.
func TestInterceptClientMessage_RefusesFunctionCall(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"read", "write"} {
		s := newTestSession(level)

		err := s.interceptClientMessage(&pgproto3.FunctionCall{Function: 2078})
		if !errors.Is(err, ErrFunctionCallNotAllowed) {
			t.Errorf("%s grant: interceptClientMessage(FunctionCall) = %v, want %v", level, err, ErrFunctionCallNotAllowed)
		}
	}
}

// Positive control: an ordinary statement still goes through the same gate.
func TestInterceptClientMessage_AllowsQuery(t *testing.T) {
	t.Parallel()

	s := newTestSession("read")

	if err := s.interceptClientMessage(&pgproto3.Query{String: "SELECT 1"}); err != nil {
		t.Errorf("interceptClientMessage(SELECT 1) = %v, want nil", err)
	}
}

// newGatedSession is a session with the relay's catch-up condition armed and
// one forwarded batch upstream has not answered yet.
func newGatedSession(level string) *Session {
	s := newTestSession(level)
	s.upstreamCaughtUp = sync.NewCond(&s.bookMu)
	s.clientSyncEpoch = 1

	return s
}

// awaitAsync runs awaitUpstreamCaughtUp on its own goroutine.
func awaitAsync(s *Session, msg pgproto3.FrontendMessage) <-chan bool {
	done := make(chan bool, 1)

	go func() { done <- s.awaitUpstreamCaughtUp(msg) }()

	return done
}

func TestAwaitUpstreamCaughtUp_HoldsReadOnlyUntilAnswered(t *testing.T) {
	t.Parallel()

	s := newGatedSession("read")
	done := awaitAsync(s, &pgproto3.Query{String: "SELECT 1"})

	select {
	case <-done:
		t.Fatal("a read-only session forwarded a batch before upstream answered the previous one")
	case <-time.After(50 * time.Millisecond):
	}

	s.bookMu.Lock()
	s.upstreamReadyEpoch++
	s.upstreamCaughtUp.Broadcast()
	s.bookMu.Unlock()

	select {
	case ok := <-done:
		if !ok {
			t.Error("awaitUpstreamCaughtUp = false once upstream caught up, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("still held after upstream answered")
	}
}

func TestAwaitUpstreamCaughtUp_ReleasedWhenRelayEnds(t *testing.T) {
	t.Parallel()

	s := newGatedSession("read")
	done := awaitAsync(s, &pgproto3.Query{String: "SELECT 1"})

	s.markUpstreamRelayEnded()

	select {
	case ok := <-done:
		if ok {
			t.Error("awaitUpstreamCaughtUp = true after the upstream leg ended, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("still held after the upstream leg ended")
	}
}

// Positive controls: a full-write session pipelines freely, and the COPY
// sub-protocol of the statement in flight is never held.
func TestAwaitUpstreamCaughtUp_DoesNotHold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		level string
		msg   pgproto3.FrontendMessage
	}{
		{"write grant", "write", &pgproto3.Query{String: "SELECT 1"}},
		{"copy data", "read", &pgproto3.CopyData{}},
		{"copy done", "read", &pgproto3.CopyDone{}},
		{"terminate", "read", &pgproto3.Terminate{}},
	}

	for _, tc := range cases {
		s := newGatedSession(tc.level)

		select {
		case ok := <-awaitAsync(s, tc.msg):
			if !ok {
				t.Errorf("%s: awaitUpstreamCaughtUp = false, want true", tc.name)
			}
		case <-time.After(time.Second):
			t.Errorf("%s: held", tc.name)
		}
	}
}
