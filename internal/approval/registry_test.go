package approval

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestResolveDeliversToParkedSession(t *testing.T) {
	r := NewRegistry()
	uid := uuid.New()

	ch, release := r.Register(uid)
	defer release()

	if !r.Resolve(Decision{QueryUID: uid, Status: "approved", ByName: "alice"}) {
		t.Fatal("resolve reported no local hold")
	}

	select {
	case d := <-ch:
		if d.Status != "approved" || d.ByName != "alice" {
			t.Fatalf("unexpected decision %+v", d)
		}
		if d.At.IsZero() {
			t.Fatal("At not stamped")
		}
	case <-time.After(time.Second):
		t.Fatal("decision not delivered")
	}
}

func TestResolveUnknownUIDIsNotAnError(t *testing.T) {
	r := NewRegistry()

	if r.Resolve(Decision{QueryUID: uuid.New(), Status: "approved"}) {
		t.Fatal("resolve claimed a hold that never existed")
	}
}

func TestSecondResolveLoses(t *testing.T) {
	r := NewRegistry()
	uid := uuid.New()

	ch, release := r.Register(uid)
	defer release()

	if !r.Resolve(Decision{QueryUID: uid, Status: "approved"}) {
		t.Fatal("first resolve failed")
	}

	if r.Resolve(Decision{QueryUID: uid, Status: "denied"}) {
		t.Fatal("second resolve should have found no hold")
	}

	d := <-ch
	if d.Status != "approved" {
		t.Fatalf("first decision must win, got %q", d.Status)
	}
}

func TestReleaseRemovesHold(t *testing.T) {
	r := NewRegistry()
	uid := uuid.New()

	_, release := r.Register(uid)
	release()

	if len(r.Pending()) != 0 {
		t.Fatal("hold leaked after release")
	}

	if r.Resolve(Decision{QueryUID: uid, Status: "approved"}) {
		t.Fatal("released hold still resolvable")
	}
}

func TestResolveAllDrainsEveryHold(t *testing.T) {
	r := NewRegistry()

	chans := make([]<-chan Decision, 0, 3)

	for i := 0; i < 3; i++ {
		ch, release := r.Register(uuid.New())
		defer release()

		chans = append(chans, ch)
	}

	got := r.ResolveAll("abandoned", "server shutting down", nil, "")
	if len(got) != 3 {
		t.Fatalf("resolved %d holds, want 3", len(got))
	}

	for i, ch := range chans {
		select {
		case d := <-ch:
			if d.Status != "abandoned" {
				t.Fatalf("hold %d got %q", i, d.Status)
			}
		case <-time.After(time.Second):
			t.Fatalf("hold %d never resolved — shutdown would hang", i)
		}
	}

	if len(r.Pending()) != 0 {
		t.Fatal("registry not empty after ResolveAll")
	}
}

func TestHeldSince(t *testing.T) {
	r := NewRegistry()
	uid := uuid.New()

	_, release := r.Register(uid)
	defer release()

	since, ok := r.HeldSince(uid)
	if !ok || time.Since(since) > time.Minute {
		t.Fatalf("HeldSince ok=%v since=%v", ok, since)
	}

	if _, ok := r.HeldSince(uuid.New()); ok {
		t.Fatal("HeldSince reported an unknown hold")
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry

	ch, release := r.Register(uuid.New())
	release()

	if r.Resolve(Decision{}) {
		t.Fatal("nil registry resolved something")
	}

	if len(r.Pending()) != 0 || len(r.ResolveAll("abandoned", "", nil, "")) != 0 {
		t.Fatal("nil registry returned holds")
	}

	select {
	case <-ch:
		t.Fatal("nil registry delivered a decision")
	default:
	}
}
