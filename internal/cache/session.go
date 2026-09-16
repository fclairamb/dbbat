package cache

import (
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// TerminationRequest is why a live session is being ended from outside it.
//
// It carries a string reason rather than a store constant because this package
// sits *below* the store (the store owns a registry, not the other way round),
// so the vocabulary — store.TerminationAdminTerminated, TerminationGrantRevoked
// — is passed through as data. The proxy stamps whatever arrives onto the
// connection row and the audit entry, which is what lets one mechanism serve
// both arms of the cross-instance poller: an admin asking for this session to
// end, and a grant revoked on another replica catching up with it.
type TerminationRequest struct {
	// Reason is the store vocabulary value (`admin_terminated`,
	// `grant_revoked`, …). Empty falls back to the session's own default.
	Reason string

	// By is the username of the human who asked, empty when nobody did.
	By string

	// Detail is that human's free text, shown to nobody but the audit log.
	Detail string
}

// SessionHandle is held by one live proxy session for its whole life. Its flag
// is flipped the instant somebody asks for that session to end — an admin
// through the terminate endpoint, or this process's poller noticing a
// termination requested on another replica — so the session's limit watchdog
// observes it on its next tick with no database round trip of its own.
//
// All methods are nil-safe: a session that could not obtain a handle (a nil
// registry in a test) treats itself as never terminated.
type SessionHandle struct {
	terminated atomic.Bool
	request    atomic.Pointer[TerminationRequest]
}

// Terminated reports whether this session has been asked to end.
func (h *SessionHandle) Terminated() bool {
	if h == nil {
		return false
	}

	return h.terminated.Load()
}

// Flag exposes the underlying atomic so a limit watchdog can poll it with a
// single atomic load alongside the byte/time checks. nil for a nil handle,
// which downstream guards read as "nothing to watch".
func (h *SessionHandle) Flag() *atomic.Bool {
	if h == nil {
		return nil
	}

	return &h.terminated
}

// Request returns why the session was asked to end, or the zero value when it
// was not (or when the reason was lost to a race with the flag, which cannot
// happen: the request is stored before the flag is raised).
func (h *SessionHandle) Request() TerminationRequest {
	if h == nil {
		return TerminationRequest{}
	}

	req := h.request.Load()
	if req == nil {
		return TerminationRequest{}
	}

	return *req
}

// SessionRegistry is the in-process fan-out from "end connection X" to the live
// proxy session serving it, keyed by **connection uid**.
//
// Every protocol already keeps a registry of its own — PostgreSQL by cancel
// key, MySQL by connection id — but each is keyed by a protocol handle, and the
// connection uid is the only identifier an admin has. This one is therefore
// protocol-agnostic and carries no database state: it maps a uid to the handle
// of the session serving it, and Terminate raises that handle's flag. The
// session's existing LimitGuard watchdog and onLimitViolation teardown (cancel
// upstream, then close both sockets) do the rest.
//
// It is a sibling of RevocationRegistry, not a replacement: revocation fans out
// from one *grant* to the several sessions under it, this one addresses a single
// session. They are both signals into the same guard.
type SessionRegistry struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]map[*SessionHandle]struct{}
}

// NewSessionRegistry creates an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[uuid.UUID]map[*SessionHandle]struct{}),
	}
}

// Register records the live session serving connUID and returns its handle.
// Deregister must be called when the session ends. Calling on a nil registry,
// or with uuid.Nil, still returns a usable (never-terminated) handle so callers
// never have to nil-check the result.
//
// The value is a *set* of handles even though a connection uid names exactly one
// session: a uid is minted per session, so a second handle under the same uid is
// a bug, and a map that silently replaced the first would leave the real session
// unreachable. A set makes that case terminate both instead of losing one.
func (r *SessionRegistry) Register(connUID uuid.UUID) *SessionHandle {
	h := &SessionHandle{}

	if r == nil || connUID == uuid.Nil {
		return h
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sessions[connUID]
	if set == nil {
		set = make(map[*SessionHandle]struct{})
		r.sessions[connUID] = set
	}

	set[h] = struct{}{}

	return h
}

// Deregister drops a handle previously returned by Register. Safe to call with
// a nil registry/handle or a handle that was never registered.
func (r *SessionRegistry) Deregister(connUID uuid.UUID, h *SessionHandle) {
	if r == nil || h == nil || connUID == uuid.Nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sessions[connUID]
	if set == nil {
		return
	}

	delete(set, h)

	if len(set) == 0 {
		delete(r.sessions, connUID)
	}
}

// Terminate asks the live session serving connUID to end, and reports whether
// one was found on this process. false means the session lives on another
// replica (or has already gone) — the caller's cue that the store, not this
// registry, is the source of truth.
//
// The request is published before the flag is raised, so a watchdog that
// observes the flag always finds the reason behind it. Handles are not
// deregistered here: the session tears itself down and Deregisters on the way
// out, exactly as the revocation path works.
//
// Terminating twice is harmless and keeps the first reason: whoever got there
// first is the one that actually ended the session.
func (r *SessionRegistry) Terminate(connUID uuid.UUID, req TerminationRequest) bool {
	if r == nil || connUID == uuid.Nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set := r.sessions[connUID]

	found := false

	for h := range set {
		if h.terminated.Load() {
			found = true

			continue
		}

		h.request.Store(&req)
		h.terminated.Store(true)

		found = true
	}

	return found
}

// Live reports whether this process is serving the session named by connUID.
// Used by the terminate endpoint to say whether the local fast path applied,
// and by tests.
func (r *SessionRegistry) Live(connUID uuid.UUID) bool {
	if r == nil || connUID == uuid.Nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.sessions[connUID]) > 0
}
