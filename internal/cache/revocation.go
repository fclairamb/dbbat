package cache

import (
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// RevocationHandle is held by a live proxy session for as long as it relies on
// a particular grant. Its flag is flipped to true the instant that grant is
// revoked, so the session's per-command check and its limit watchdog observe
// the revocation without a database round-trip on every query.
//
// All methods are nil-safe: a session that could not obtain a handle (e.g. a
// nil registry in a test) treats itself as never-revoked.
type RevocationHandle struct {
	revoked atomic.Bool
}

// Revoked reports whether the grant backing this handle has been revoked.
func (h *RevocationHandle) Revoked() bool {
	if h == nil {
		return false
	}

	return h.revoked.Load()
}

// Flag exposes the underlying atomic flag so a limit watchdog can poll it
// cheaply (a single atomic load) alongside the byte/time checks. Returns nil
// for a nil handle, which downstream guards treat as "no revocation to watch".
func (h *RevocationHandle) Flag() *atomic.Bool {
	if h == nil {
		return nil
	}

	return &h.revoked
}

// RevocationRegistry is an in-process fan-out from the API's grant-revoke path
// to the live proxy sessions that authenticated under those grants. It lets a
// revocation take effect on already-established connections — blocking their
// next query and tearing the session down — instead of only being consulted at
// connect time.
//
// It carries no database state: it maps a grant UID to the set of live session
// handles depending on it. Revoke flips their flags; the sessions' existing
// LimitGuard watchdog and checkQuotas paths do the rest.
type RevocationRegistry struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]map[*RevocationHandle]struct{}
}

// NewRevocationRegistry creates an empty registry.
func NewRevocationRegistry() *RevocationRegistry {
	return &RevocationRegistry{
		sessions: make(map[uuid.UUID]map[*RevocationHandle]struct{}),
	}
}

// Register records a live session that relies on grantUID and returns its
// handle. Deregister must be called when the session ends. Calling on a nil
// registry, or with uuid.Nil, still returns a usable (never-revoked) handle so
// callers never have to nil-check the result.
func (r *RevocationRegistry) Register(grantUID uuid.UUID) *RevocationHandle {
	h := &RevocationHandle{}

	if r == nil || grantUID == uuid.Nil {
		return h
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sessions[grantUID]
	if set == nil {
		set = make(map[*RevocationHandle]struct{})
		r.sessions[grantUID] = set
	}

	set[h] = struct{}{}

	return h
}

// Deregister drops a handle previously returned by Register. Safe to call with
// a nil registry/handle or a handle that was never registered.
func (r *RevocationRegistry) Deregister(grantUID uuid.UUID, h *RevocationHandle) {
	if r == nil || h == nil || grantUID == uuid.Nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sessions[grantUID]
	if set == nil {
		return
	}

	delete(set, h)

	if len(set) == 0 {
		delete(r.sessions, grantUID)
	}
}

// Revoke flips the revoked flag on every live session bound to grantUID and
// returns the number of sessions signaled. Safe to call for a grant with no
// live sessions (returns 0). It does not deregister the handles — the sessions
// tear themselves down and Deregister on the way out.
func (r *RevocationRegistry) Revoke(grantUID uuid.UUID) int {
	if r == nil || grantUID == uuid.Nil {
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sessions[grantUID]

	n := 0
	for h := range set {
		h.revoked.Store(true)
		n++
	}

	return n
}
