// Package approval holds the process-local registry of parked queries.
//
// A hold is a proxy session goroutine blocked mid-statement waiting for a
// human. The registry is what lets the API handler on the same replica — or a
// LISTEN/NOTIFY message from another one — wake that goroutine up.
//
// The registry only *delivers* decisions. It deliberately does not decide who
// may approve (that is the API's job, which has the user and their groups) and
// does not persist anything (that is the store's job, whose compare-and-set on
// approval_status = 'pending' is the real source of truth). Keeping those apart
// is what makes double-resolution safe: the DB update decides the winner, the
// registry just carries the news.
package approval

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Decision is the outcome delivered to a parked session.
type Decision struct {
	// QueryUID identifies the hold. The waiting session asserts this equals
	// its own pending uid before acting on it: without that check, a client
	// able to influence timing could have somebody else's approval released
	// against its statement (TOCTOU substitution).
	QueryUID uuid.UUID
	// Status is one of store.ApprovalApproved / ApprovalDenied /
	// ApprovalAbandoned.
	Status string
	// By is the resolving user, nil for system resolutions (client
	// disconnect, shutdown).
	By *uuid.UUID
	// ByName is the resolver's display name, carried so every watcher sees
	// *who* unblocked a query and not merely that it unblocked.
	ByName string
	// Reason is the approver-supplied justification, surfaced to the client
	// in the protocol-native deny error.
	Reason string
	// At is when the decision was taken.
	At time.Time
}

// hold is one parked statement.
type hold struct {
	ch        chan Decision
	startedAt time.Time
}

// Registry tracks parked statements on this replica.
type Registry struct {
	mu    sync.Mutex
	holds map[uuid.UUID]*hold
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{holds: make(map[uuid.UUID]*hold)}
}

// Register parks a query. The returned channel receives at most one decision;
// release must be called (deferred) when the session stops waiting, whatever
// the reason — otherwise the hold leaks and shutdown would block on it.
func (r *Registry) Register(queryUID uuid.UUID) (<-chan Decision, func()) {
	if r == nil {
		ch := make(chan Decision, 1)

		return ch, func() {}
	}

	// Buffered: Resolve must never block on a session that has already given
	// up (client disconnected) but not yet released.
	h := &hold{ch: make(chan Decision, 1), startedAt: time.Now()}

	r.mu.Lock()
	r.holds[queryUID] = h
	r.mu.Unlock()

	return h.ch, func() {
		r.mu.Lock()
		if cur, ok := r.holds[queryUID]; ok && cur == h {
			delete(r.holds, queryUID)
		}
		r.mu.Unlock()
	}
}

// Resolve delivers a decision to a locally parked session. It reports whether
// a hold existed here — false simply means the session lives on another
// replica (or already gave up), which is not an error.
func (r *Registry) Resolve(d Decision) bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	h, ok := r.holds[d.QueryUID]

	if ok {
		delete(r.holds, d.QueryUID)
	}
	r.mu.Unlock()

	if !ok {
		return false
	}

	if d.At.IsZero() {
		d.At = time.Now()
	}

	select {
	case h.ch <- d:
		return true
	default:
		// Already resolved by a racing path; the first decision wins.
		return false
	}
}

// Pending returns the uids parked on this replica, oldest first is not
// guaranteed — callers use this for shutdown draining and telemetry, not
// ordering.
func (r *Registry) Pending() []uuid.UUID {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]uuid.UUID, 0, len(r.holds))
	for uid := range r.holds {
		out = append(out, uid)
	}

	return out
}

// HeldSince returns when a hold started, if it is parked here.
func (r *Registry) HeldSince(queryUID uuid.UUID) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	h, ok := r.holds[queryUID]
	if !ok {
		return time.Time{}, false
	}

	return h.startedAt, true
}

// ResolveAll delivers the same status to every hold on this replica and
// returns the uids it resolved. Used on shutdown: draining must explicitly
// abandon parked queries rather than hang the shutdown or, worse, silently let
// them through.
func (r *Registry) ResolveAll(status, reason string, by *uuid.UUID, byName string) []uuid.UUID {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	holds := r.holds
	r.holds = make(map[uuid.UUID]*hold)
	r.mu.Unlock()

	now := time.Now()
	resolved := make([]uuid.UUID, 0, len(holds))

	for uid, h := range holds {
		select {
		case h.ch <- Decision{QueryUID: uid, Status: status, Reason: reason, By: by, ByName: byName, At: now}:
			resolved = append(resolved, uid)
		default:
		}
	}

	return resolved
}
