package cache

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestSessionRegistry_TerminateSignalsRegisteredSession(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()
	conn := uuid.New()

	h := r.Register(conn)

	if h.Terminated() {
		t.Fatal("handle should not be terminated before Terminate")
	}

	req := TerminationRequest{Reason: "admin_terminated", By: "alice", Detail: "runaway query"}

	if !r.Terminate(conn, req) {
		t.Fatal("Terminate should report the session as live on this process")
	}

	if !h.Terminated() {
		t.Fatal("handle should be terminated after Terminate")
	}

	if got := h.Request(); got != req {
		t.Fatalf("Request() = %+v, want %+v", got, req)
	}
}

func TestSessionRegistry_TerminateOnlyAffectsMatchingConnection(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()
	connA := uuid.New()
	connB := uuid.New()

	ha := r.Register(connA)
	hb := r.Register(connB)

	if !r.Terminate(connA, TerminationRequest{Reason: "admin_terminated"}) {
		t.Fatal("Terminate(connA) should have found a session")
	}

	if !ha.Terminated() {
		t.Fatal("connA handle should be terminated")
	}

	if hb.Terminated() {
		t.Fatal("connB handle must be unaffected by terminating connA")
	}
}

func TestSessionRegistry_TerminateUnknownConnectionReportsNotLocal(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()

	// The cross-instance case: the session is on another replica, so this
	// process has nothing to signal and must say so rather than claiming
	// success.
	if r.Terminate(uuid.New(), TerminationRequest{Reason: "admin_terminated"}) {
		t.Fatal("Terminate(unknown) should report false")
	}
}

func TestSessionRegistry_DeregisteredSessionNotSignaled(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()
	conn := uuid.New()

	h := r.Register(conn)
	r.Deregister(conn, h)

	if r.Terminate(conn, TerminationRequest{Reason: "admin_terminated"}) {
		t.Fatal("Terminate after Deregister should report false")
	}

	if h.Terminated() {
		t.Fatal("deregistered handle must not be terminated")
	}

	if r.Live(conn) {
		t.Fatal("Live() should be false after Deregister")
	}
}

func TestSessionRegistry_TerminateTwiceKeepsFirstReason(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()
	conn := uuid.New()

	h := r.Register(conn)

	first := TerminationRequest{Reason: "admin_terminated", By: "alice"}
	if !r.Terminate(conn, first) {
		t.Fatal("first Terminate should have found the session")
	}

	// The poller re-reads the same request row every tick until the session is
	// gone, so a second signal is the normal case — it must not rewrite the
	// reason that actually ended the session, and it must report "nothing new"
	// so the poller does not log a termination every two seconds.
	if r.Terminate(conn, TerminationRequest{Reason: "grant_revoked", By: "bob"}) {
		t.Fatal("second Terminate should report false: nothing new was signaled")
	}

	if !r.Live(conn) {
		t.Fatal("the session is still registered, so Live() should be true")
	}

	if got := h.Request(); got != first {
		t.Fatalf("Request() = %+v, want the first request %+v", got, first)
	}
}

func TestSessionRegistry_FlagIsTheGuardsView(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()
	conn := uuid.New()

	h := r.Register(conn)

	flag := h.Flag()
	if flag == nil {
		t.Fatal("Flag() should expose the atomic for the limit guard")
	}

	if flag.Load() {
		t.Fatal("flag should start false")
	}

	r.Terminate(conn, TerminationRequest{Reason: "admin_terminated"})

	if !flag.Load() {
		t.Fatal("flag should be raised by Terminate")
	}
}

func TestSessionRegistry_NilSafety(t *testing.T) {
	t.Parallel()

	var r *SessionRegistry // nil registry

	h := r.Register(uuid.New())
	if h == nil {
		t.Fatal("Register on nil registry returned nil handle")
	}

	if h.Terminated() {
		t.Fatal("handle from nil registry should not be terminated")
	}

	// These must not panic.
	r.Deregister(uuid.New(), h)

	if r.Terminate(uuid.New(), TerminationRequest{Reason: "admin_terminated"}) {
		t.Fatal("Terminate on nil registry should report false")
	}

	if r.Live(uuid.New()) {
		t.Fatal("Live on nil registry should report false")
	}

	// uuid.Nil is ignored by a real registry.
	reg := NewSessionRegistry()

	hn := reg.Register(uuid.Nil)
	if hn == nil {
		t.Fatal("Register(uuid.Nil) returned nil handle")
	}

	if reg.Terminate(uuid.Nil, TerminationRequest{Reason: "admin_terminated"}) {
		t.Fatal("Terminate(uuid.Nil) should report false")
	}
}

func TestSessionHandle_NilReceiver(t *testing.T) {
	t.Parallel()

	var h *SessionHandle

	if h.Terminated() {
		t.Fatal("nil handle Terminated() should be false")
	}

	if h.Flag() != nil {
		t.Fatal("nil handle Flag() should be nil")
	}

	if got := h.Request(); got != (TerminationRequest{}) {
		t.Fatalf("nil handle Request() = %+v, want the zero value", got)
	}
}

func TestSessionRegistry_ConcurrentRegisterTerminateDeregister(t *testing.T) {
	t.Parallel()

	r := NewSessionRegistry()
	conn := uuid.New()

	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			h := r.Register(conn)
			_ = h.Terminated()
			_ = h.Request()
			r.Deregister(conn, h)
		}()
	}

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = r.Terminate(conn, TerminationRequest{Reason: "admin_terminated", By: "alice"})
			_ = r.Live(conn)
		}()
	}

	wg.Wait()
}
