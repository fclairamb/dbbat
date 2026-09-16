package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrConnectionAlreadyClosed is returned when a termination is asked of a
// session that has already ended. Distinct from ErrConnectionNotFound because
// the two are different answers to the caller: one is "no such session", the
// other "there is nothing left to end" — 404 and 409 respectively.
var ErrConnectionAlreadyClosed = errors.New("connection is already closed")

// TerminationPollInterval is how often each process asks the store whether any
// of the sessions *it* owns has been asked to end.
//
// Two seconds, and deliberately a poll rather than LISTEN/NOTIFY. NOTIFY would
// make the remote case instant, at the cost of a dedicated connection per
// replica plus reconnect handling; this query is two index lookups bounded by
// the replica's own live sessions, so it is cheap enough to leave running for
// the life of the process. The admin-visible cost is up to two seconds between
// pressing the button and the session dropping — and only when the session is
// on *another* replica, since the replica serving the API call signals its own
// registry directly.
const TerminationPollInterval = 2 * time.Second

// RequestConnectionTermination records an admin's request to end one live
// session, and returns the row it marked.
//
// The request is a row rather than a message because the replica serving the
// API call is not necessarily the one serving the session: connections.run_id
// says who is, and a run id is minted in memory, so there is no in-process
// channel between them. The owner picks this up on its next
// TerminationPollInterval tick (see PendingTerminations) — or immediately, when
// the owner happens to be us and the handler signals the registry directly.
//
// A repeat request overwrites the previous one: this is an intent, the last
// admin to press the button is the one whose reason should be recorded, and the
// session it names is by definition still running or the call would have been
// refused.
//
// Refused with ErrConnectionAlreadyClosed for a session that has ended and
// ErrConnectionNotFound for a uid that names nothing.
func (s *Store) RequestConnectionTermination(
	ctx context.Context, uid, requestedBy uuid.UUID, reason string,
) (*Connection, error) {
	var marked []Connection

	err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Where("disconnected_at IS NULL").
		Set("terminate_requested_at = ?", time.Now()).
		Set("terminate_requested_by = ?", requestedBy).
		Set("terminate_reason = ?", nullableText(reason)).
		Returning("uid, user_id, database_id, host(source_ip) AS source_ip, connected_at, "+
			"disconnected_at, instance_id, run_id, grant_uid, "+
			"terminate_requested_at, terminate_requested_by, terminate_reason").
		Scan(ctx, &marked)
	if err != nil {
		return nil, fmt.Errorf("failed to request the connection termination: %w", err)
	}

	if len(marked) > 0 {
		return &marked[0], nil
	}

	// Nothing matched: either the session is closed or the uid is unknown, and
	// the caller answers those differently. One extra read, only on the path
	// that is about to fail.
	if _, err := s.GetConnectionByUID(ctx, uid); err != nil {
		return nil, err
	}

	return nil, ErrConnectionAlreadyClosed
}

// PendingTermination is one session this process owns that should be ended.
type PendingTermination struct {
	ConnectionUID uuid.UUID `bun:"connection_uid"`

	// Reason is the vocabulary value to record — TerminationAdminTerminated for
	// a requested termination, TerminationGrantRevoked for the revocation arm.
	Reason string `bun:"reason"`

	// RequestedBy is the requesting admin's username, empty for the revocation
	// arm (nobody asked for *this session* to end; a grant was withdrawn).
	RequestedBy string `bun:"requested_by"`

	// Detail is the requesting admin's free text, empty when they gave none.
	Detail string `bun:"detail"`
}

// PendingTerminations returns the sessions owned by *this run* that should be
// torn down, from the two sources that can ask for it.
//
// The first arm is the terminate endpoint's request row. The second is the fix
// for a correctness bug in shipped code: revoking a grant only ever signaled
// cache.RevocationRegistry, which is in-process, so in a multi-replica
// deployment a revocation ended the sessions that happened to live on the
// replica serving the API call and left every other one running until it
// expired. GetActiveGrant runs only at connect and LimitGuard compares
// expires_at, never revoked_at, so nothing else caught it. With this arm the
// in-process registry becomes a fast path and the store is the source of truth.
//
// Scoped to run_id, never to instance_id: an instance id can be shared by
// several replicas (a pinned DBB_INSTANCE_ID, config.FallbackInstanceID), and a
// process must only act on the sessions it is actually serving — signaling the
// registry for somebody else's uid would find nothing anyway, but asking for it
// is how a poll turns into a store-wide scan.
//
// Cost: the first arm is served by idx_connections_terminate_requested, partial
// on exactly its predicate, so it reads the handful of rows that are actually
// pending. The second is bounded by the live sessions in the store
// (idx_connections_open_uid, partial on disconnected_at IS NULL) — the same
// bound the crash reconcile and the chain-stamp refresh already accept.
func (s *Store) PendingTerminations(ctx context.Context) ([]PendingTermination, error) {
	if s.runID == "" {
		return nil, nil
	}

	var pending []PendingTermination

	const query = `
		SELECT c.uid AS connection_uid,
		       ? AS reason,
		       COALESCE(u.username, '') AS requested_by,
		       COALESCE(c.terminate_reason, '') AS detail
		  FROM connections c
		  LEFT JOIN users u ON u.uid = c.terminate_requested_by
		 WHERE c.run_id = ?
		   AND c.terminate_requested_at IS NOT NULL
		   AND c.disconnected_at IS NULL
		UNION ALL
		SELECT c.uid AS connection_uid,
		       ? AS reason,
		       '' AS requested_by,
		       '' AS detail
		  FROM connections c
		  JOIN access_grants g ON g.uid = c.grant_uid
		 WHERE c.run_id = ?
		   AND c.disconnected_at IS NULL
		   AND g.revoked_at IS NOT NULL`

	err := s.db.NewRaw(query,
		TerminationAdminTerminated, s.runID,
		TerminationGrantRevoked, s.runID).
		Scan(ctx, &pending)
	if err != nil {
		return nil, fmt.Errorf("failed to list the pending session terminations: %w", err)
	}

	return pending, nil
}

// nullableText maps empty free text to a SQL NULL, so "the admin gave no
// reason" is not stored as an empty string that reads like one.
func nullableText(s string) any {
	if s == "" {
		return nil
	}

	return s
}
