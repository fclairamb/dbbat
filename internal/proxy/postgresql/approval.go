package postgresql

import (
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// pgCancelRequestCode is the protocol "version" that marks a CancelRequest.
// PostgreSQL sends it on a *separate* TCP connection, which is exactly why a
// parked statement stays cancellable: nothing about the held session's socket
// is involved.
const pgCancelRequestCode = 80877102

// cancelKey identifies a session by the BackendKeyData it handed the client.
// The secret is held as a string rather than a uint32 because pgproto3 models
// it as a variable-length byte slice (protocol 3.2 widened it).
type cancelKey struct {
	processID uint32
	secretKey string
}

// cancelRegistry maps the cancel keys dbbat forwarded to clients back to the
// sessions that own them, so an out-of-band CancelRequest can reach a session
// that is parked on a human.
type cancelRegistry struct {
	mu       sync.Mutex
	sessions map[cancelKey]*Session
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{sessions: make(map[cancelKey]*Session)}
}

func (r *cancelRegistry) register(key cancelKey, s *Session) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.sessions[key] = s
	r.mu.Unlock()
}

func (r *cancelRegistry) unregister(key cancelKey) {
	if r == nil {
		return
	}

	r.mu.Lock()
	delete(r.sessions, key)
	r.mu.Unlock()
}

func (r *cancelRegistry) lookup(key cancelKey) (*Session, bool) {
	if r == nil {
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[key]

	return s, ok
}

// noteCancelKey remembers the BackendKeyData being forwarded to the client so
// a later CancelRequest carrying it can be routed back here.
func (s *Session) noteCancelKey(msg *pgproto3.BackendKeyData) {
	if msg == nil || s.cancels == nil {
		return
	}

	key := cancelKey{processID: msg.ProcessID, secretKey: string(msg.SecretKey)}

	s.cancelKeyMu.Lock()
	s.cancelKey = &key
	s.cancelKeyMu.Unlock()

	s.cancels.register(key, s)
}

// releaseCancelKey drops the registration on session teardown.
func (s *Session) releaseCancelKey() {
	if s.cancels == nil {
		return
	}

	s.cancelKeyMu.Lock()
	key := s.cancelKey
	s.cancelKey = nil
	s.cancelKeyMu.Unlock()

	if key != nil {
		s.cancels.unregister(*key)
	}
}

// setHeldQuery records (or clears) the uid of the statement currently parked
// on a human, so a CancelRequest arriving on another connection can end it.
func (s *Session) setHeldQuery(uid uuid.UUID) {
	s.heldMu.Lock()
	s.heldQueryUID = uid
	s.heldMu.Unlock()
}

func (s *Session) heldQuery() uuid.UUID {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	return s.heldQueryUID
}

// cancelHeldQuery resolves a parked statement as abandoned in response to a
// PostgreSQL CancelRequest. Reports whether anything was actually parked.
func (s *Session) cancelHeldQuery() bool {
	uid := s.heldQuery()
	if uid == uuid.Nil || s.approvalDeps.Registry == nil {
		return false
	}

	return s.approvalDeps.Registry.Resolve(approval.Decision{
		QueryUID: uid,
		Status:   store.ApprovalAbandoned,
		Reason:   "canceled by the client (CancelRequest)",
		At:       time.Now(),
	})
}

// holdIfNeeded runs the approval gate for one statement. It returns the uid of
// the pending row the gate created (uuid.Nil when no hold was needed) and an
// error when the statement must not be forwarded.
//
// It is called *after* the static validators — cheap deterministic denies stay
// cheap — and *before* anything reaches upstream.
func (s *Session) holdIfNeeded(sql string, params *store.QueryParameters) (uuid.UUID, error) {
	if !s.approvalGate.Active() {
		return uuid.Nil, nil
	}

	pattern, matched := s.approvalGate.Match(sql)
	if !matched {
		return uuid.Nil, nil
	}

	// Park the client socket: the session goroutine is about to stop reading
	// it, so without an active read-watch a client FIN would sit unnoticed and
	// the hold would never end.
	var gone <-chan struct{}

	if s.watched != nil {
		gone = s.watched.Park()
		defer s.watched.Unpark()
	}

	return s.approvalGate.Hold(s.ctx, shared.HoldRequest{
		SQL:        sql,
		Params:     params,
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: gone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// handleCancelRequest routes an out-of-band CancelRequest to the session that
// owns the key, ending any statement it has parked on a human and forwarding
// the cancel upstream so a genuinely running query is canceled too.
func (s *Session) handleCancelRequest(msg *pgproto3.CancelRequest) {
	key := cancelKey{processID: msg.ProcessID, secretKey: string(msg.SecretKey)}

	target, ok := s.cancels.lookup(key)
	if !ok {
		s.logger.DebugContext(s.ctx, "cancel request for an unknown backend key")

		return
	}

	if target.cancelHeldQuery() {
		s.logger.InfoContext(s.ctx, "cancel request released an approval hold",
			slog.String("connection_uid", target.connectionUID.String()))
	}

	target.forwardCancelUpstream(msg)
}

// forwardCancelUpstream opens a throwaway connection to the target server and
// relays the CancelRequest. Best-effort: PostgreSQL itself gives no
// acknowledgement for a cancel, so a failure here is logged and dropped.
func (s *Session) forwardCancelUpstream(msg *pgproto3.CancelRequest) {
	if s.database == nil {
		return
	}

	addr := net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))

	conn, err := net.DialTimeout("tcp", addr, cancelForwardTimeout)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to forward cancel upstream", slog.Any("error", err))

		return
	}

	defer func() { _ = conn.Close() }()

	_ = conn.SetWriteDeadline(time.Now().Add(cancelForwardTimeout))

	frame, err := msg.Encode(nil)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to encode cancel request", slog.Any("error", err))

		return
	}

	if _, err := conn.Write(frame); err != nil {
		s.logger.DebugContext(s.ctx, "failed to write cancel upstream", slog.Any("error", err))
	}
}

// cancelForwardTimeout bounds the throwaway upstream cancel connection.
const cancelForwardTimeout = 5 * time.Second
