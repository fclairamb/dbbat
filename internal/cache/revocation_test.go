package cache

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestRevocationRegistry_RevokeSignalsRegisteredSessions(t *testing.T) {
	t.Parallel()

	r := NewRevocationRegistry()
	grant := uuid.New()

	h1 := r.Register(grant)
	h2 := r.Register(grant)

	if h1.Revoked() || h2.Revoked() {
		t.Fatal("handles should not be revoked before Revoke")
	}

	n := r.Revoke(grant)
	if n != 2 {
		t.Fatalf("Revoke signaled %d sessions, want 2", n)
	}

	if !h1.Revoked() || !h2.Revoked() {
		t.Fatal("both handles should be revoked after Revoke")
	}
}

func TestRevocationRegistry_RevokeOnlyAffectsMatchingGrant(t *testing.T) {
	t.Parallel()

	r := NewRevocationRegistry()
	grantA := uuid.New()
	grantB := uuid.New()

	ha := r.Register(grantA)
	hb := r.Register(grantB)

	if got := r.Revoke(grantA); got != 1 {
		t.Fatalf("Revoke(grantA) = %d, want 1", got)
	}

	if !ha.Revoked() {
		t.Fatal("grantA handle should be revoked")
	}

	if hb.Revoked() {
		t.Fatal("grantB handle must be unaffected by revoking grantA")
	}
}

func TestRevocationRegistry_DeregisteredSessionNotSignaled(t *testing.T) {
	t.Parallel()

	r := NewRevocationRegistry()
	grant := uuid.New()

	h := r.Register(grant)
	r.Deregister(grant, h)

	if got := r.Revoke(grant); got != 0 {
		t.Fatalf("Revoke after Deregister signaled %d, want 0", got)
	}

	if h.Revoked() {
		t.Fatal("deregistered handle must not be revoked")
	}
}

func TestRevocationRegistry_RevokeUnknownGrantIsNoOp(t *testing.T) {
	t.Parallel()

	r := NewRevocationRegistry()

	if got := r.Revoke(uuid.New()); got != 0 {
		t.Fatalf("Revoke(unknown) = %d, want 0", got)
	}
}

func TestRevocationRegistry_NilSafety(t *testing.T) {
	t.Parallel()

	var r *RevocationRegistry // nil registry

	// A nil registry still yields a usable, never-revoked handle.
	h := r.Register(uuid.New())
	if h == nil {
		t.Fatal("Register on nil registry returned nil handle")
	}

	if h.Revoked() {
		t.Fatal("handle from nil registry should not be revoked")
	}

	// These must not panic.
	r.Deregister(uuid.New(), h)
	if got := r.Revoke(uuid.New()); got != 0 {
		t.Fatalf("Revoke on nil registry = %d, want 0", got)
	}

	// uuid.Nil grant is ignored by a real registry.
	realReg := NewRevocationRegistry()
	hn := realReg.Register(uuid.Nil)
	if hn == nil {
		t.Fatal("Register(uuid.Nil) returned nil handle")
	}

	if got := realReg.Revoke(uuid.Nil); got != 0 {
		t.Fatalf("Revoke(uuid.Nil) = %d, want 0", got)
	}
}

func TestRevocationHandle_NilReceiver(t *testing.T) {
	t.Parallel()

	var h *RevocationHandle

	if h.Revoked() {
		t.Fatal("nil handle Revoked() should be false")
	}

	if h.Flag() != nil {
		t.Fatal("nil handle Flag() should be nil")
	}
}

func TestRevocationRegistry_ConcurrentRegisterRevokeDeregister(t *testing.T) {
	t.Parallel()

	r := NewRevocationRegistry()
	grant := uuid.New()

	var wg sync.WaitGroup

	// Concurrent registers + deregisters racing against repeated revokes.
	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			h := r.Register(grant)
			_ = h.Revoked()
			r.Deregister(grant, h)
		}()
	}

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = r.Revoke(grant)
		}()
	}

	wg.Wait()
}
