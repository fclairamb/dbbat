package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// The session audit entries.
//
// `connections` is not chained, and deliberately so: a whole-session delete
//
//	DELETE FROM connections WHERE uid = '…';
//
// cascades through `queries` and `query_rows`, which is the cheapest attack on
// the query history. Chaining that table would have to be reconciled against the
// retention sweep that deletes from it — the very problem the query chain is
// split per connection to avoid — so instead every session open and every
// session close writes an entry into `audit_log`, which is already HMAC-chained
// and is never reaped by retention.
//
// The evidence therefore lives in a table the delete does not touch: the session
// vanishes, its two entries do not, and they carry enough of the row's immutable
// identity — who, from where, against which database, when, under which run —
// to say what was removed. The close entry additionally carries the session's
// query_chain_mac, so the sealed record points at the query chain it owned.
//
// What this does *not* buy is a sealed `connections` row. `connected_at` on the
// row itself is still a plain column anyone with write access can backdate; see
// docs/audit-chain.md, "What it proves, and what it does not".
const (
	// AuditEventConnectionOpened is written once per session, after the
	// connection row exists.
	AuditEventConnectionOpened = "connection.opened"

	// AuditEventConnectionClosed is written once per session, after
	// disconnected_at is committed — by whichever writer got there, which the
	// entry's closed_by records.
	AuditEventConnectionClosed = "connection.closed"

	// AuditEventConnectionTerminated is written when *dbbat* ended a session
	// rather than the client — a statement over its time limit, a grant that
	// expired or was revoked mid-flight, a quota crossed, and later an admin
	// pulling the plug.
	//
	// It is written *in addition to* connection.closed, not instead of it: the
	// close entry is the one that seals the query chain, and folding the two
	// would mean either losing that seal or duplicating it. This entry carries
	// the *why*, including the statement that caused it, which is what an
	// operator reading the audit page after a killed session actually needs.
	AuditEventConnectionTerminated = "connection.terminated"

	// AuditEventConnectionTerminateRequested is written when an admin asks for
	// a session to end, by the replica that served the API call.
	//
	// Separate from connection.terminated, and not one of the session events
	// below: this is a control-plane action with a PerformedBy, it happens
	// whether or not the session is still there to be ended (it can close on
	// its own in the couple of seconds before its owner polls), and it is
	// written by a different process than the one that finally tears the
	// session down. An admin action that left no trace unless it succeeded
	// would be the wrong way round, so it belongs in the ordinary audit
	// listing alongside grant.revoked.
	AuditEventConnectionTerminateRequested = "connection.terminate_requested"
)

// Why dbbat ended a session, as written to connections.termination_reason and
// to the connection.terminated audit entry. A small, closed vocabulary: the UI
// and any log-based alerting switch on these exact strings.
const (
	// TerminationStatementTimeout — one statement ran past the grant's
	// per-statement limit (plus the watchdog grace).
	TerminationStatementTimeout = "statement_timeout"
	// TerminationGrantExpired — the grant's window closed mid-session.
	TerminationGrantExpired = "grant_expired"
	// TerminationQuotaExceeded — the grant's byte or query quota was crossed
	// mid-session.
	TerminationQuotaExceeded = "quota_exceeded"
	// TerminationGrantRevoked — an admin revoked the grant mid-session.
	TerminationGrantRevoked = "grant_revoked"
	// TerminationAdminTerminated — an admin ended this specific session,
	// through POST /connections/{uid}/terminate.
	//
	// Not the same thing as TerminationGrantRevoked, and deliberately: the
	// grant is untouched, so the user may reconnect immediately. It ends one
	// session, not an access.
	TerminationAdminTerminated = "admin_terminated"
	// TerminationInstanceLost — nobody ended this session; the process serving
	// it died, and the reconcile closed the row on its behalf.
	//
	// It is what keeps termination_reason from having unexplained NULLs on
	// closed rows: without it a crash-orphaned session reads exactly like a
	// client that hung up politely. It is never written by a session — by
	// definition there is none left to write it — only by
	// Store.closeOrphans.
	TerminationInstanceLost = "instance_lost"
)

// ValidTerminationReasons is the closed vocabulary above, for validation and
// for the API's documented enum.
var ValidTerminationReasons = []string{
	TerminationStatementTimeout,
	TerminationGrantExpired,
	TerminationQuotaExceeded,
	TerminationGrantRevoked,
	TerminationAdminTerminated,
	TerminationInstanceLost,
}

// SessionAuditEventTypes are the audit events one *session* produces, as opposed
// to the control-plane changes the rest of `audit_log` records.
//
// They are excluded from an unfiltered ListAuditEvents, and that is a deliberate
// call rather than an oversight. A proxy that serves ten thousand sessions a day
// writes twenty thousand of these against a handful of grant, user and key
// changes; folded into the same listing they would push every control-plane
// event off the first page within seconds, and the audit page is where an
// operator goes to see who changed access. The same information already has a
// purpose-built surface — the connections list, which shows live counters these
// entries deliberately omit — so nothing is hidden, only unmixed. Asking for one
// by name (`?event_type=connection.closed`) returns it, and neither the chain
// nor `dbbat audit verify` knows the difference: these are ordinary chained rows.
var SessionAuditEventTypes = []string{
	AuditEventConnectionOpened,
	AuditEventConnectionClosed,
	AuditEventConnectionTerminated,
}

// Who wrote a connection.closed entry. It is part of the record because the two
// mean different things to a reader: a clean teardown sealed what the session
// actually wrote, while a reconcile sealed whatever survived at reconcile time.
const (
	connectionClosedBySession   = "session"
	connectionClosedByReconcile = "reconcile"
)

// connectionAuditTimeout bounds one session-audit write. The write runs on a
// context detached from the caller's (see writeConnectionAudit), so it needs a
// deadline of its own or a wedged store would leak the goroutine closing a
// session.
const connectionAuditTimeout = 15 * time.Second

// connectionAuditDetails is the `details` document of a session audit entry.
//
// It carries the connection row's **immutable identity** and nothing else. The
// mutable counters — last_activity_at, queries, bytes_transferred — are left out
// for the same reason the query chain leaves out a statement's outcome: they
// keep changing after the entry is sealed, so recording them would only ever
// produce a record that disagrees with the row.
type connectionAuditDetails struct {
	ConnectionUID string `json:"connection_uid"`
	UserID        string `json:"user_id"`
	DatabaseID    string `json:"database_id"`
	SourceIP      string `json:"source_ip"`
	ConnectedAt   string `json:"connected_at"`
	InstanceID    string `json:"instance_id"`
	RunID         string `json:"run_id,omitempty"`
	GrantUID      string `json:"grant_uid,omitempty"`

	// Close-only. ClosedBy is one of the constants above; the three chain
	// fields are the stamp the close (or the reconcile) sealed onto the row, so
	// the audit entry points at the query chain this session owned even after
	// the row carrying it is gone.
	DisconnectedAt string `json:"disconnected_at,omitempty"`
	ClosedBy       string `json:"closed_by,omitempty"`

	// Termination-only: why dbbat ended the session, which statement was in
	// flight when it did, and — for a statement timeout — the limit that was
	// crossed and how long the statement had actually been running.
	TerminationReason      string `json:"termination_reason,omitempty"`
	TerminatedBy           string `json:"terminated_by,omitempty"`
	QueryUID               string `json:"query_uid,omitempty"`
	LimitSeconds           string `json:"limit,omitempty"`
	ObservedDuration       string `json:"observed_duration,omitempty"`
	TerminationDetails     string `json:"detail,omitempty"`
	QueryChainMAC          string `json:"query_chain_mac,omitempty"`
	QueryChainLen          *int64 `json:"query_chain_len,omitempty"`
	QueryChainStampVersion *int16 `json:"query_chain_stamp_version,omitempty"`
}

// connectionAuditColumns is the projection every session audit entry is built
// from: the immutable identity plus the close's outcome.
//
// source_ip goes through host() rather than the ::text cast the rest of the
// store uses. The column is `inet` and the field is a string either way, but the
// inet→text *cast* always appends the mask length ("10.0.0.1/32") while the
// value handed to CreateConnection — and therefore the one the session-open
// entry carries — is the bare address. host() is what makes the open and close
// entries of one session agree on where it came from.
const connectionAuditColumns = "uid, user_id, database_id, host(source_ip) AS source_ip, connected_at, " +
	"disconnected_at, instance_id, run_id, grant_uid, termination_reason, " +
	"query_chain_mac, query_chain_len, query_chain_stamp_version"

// connectionOpenedEvent builds the entry written when a session starts.
//
// UserID is the person who connected. PerformedBy stays nil: nobody *performed*
// this the way an admin performs a grant revocation, and filling it with the
// same user would make the audit page read as if they had acted on themselves.
func connectionOpenedEvent(conn *Connection) (*AuditEvent, error) {
	return connectionAuditEvent(AuditEventConnectionOpened, conn, connectionAuditDetails{
		ConnectionUID: conn.UID.String(),
		UserID:        conn.UserID.String(),
		DatabaseID:    conn.DatabaseID.String(),
		SourceIP:      conn.SourceIP,
		ConnectedAt:   auditTimestamp(conn.ConnectedAt),
		InstanceID:    conn.InstanceID,
		RunID:         derefString(conn.RunID),
		GrantUID:      optionalUUIDString(conn.GrantUID),
	})
}

// connectionClosedEvent builds the entry written when a session ends, from the
// row as it stands *after* the close committed — so disconnected_at and the
// sealed query-chain stamp are the ones the row actually carries.
func connectionClosedEvent(conn *Connection, closedBy string) (*AuditEvent, error) {
	details := connectionAuditDetails{
		ConnectionUID: conn.UID.String(),
		UserID:        conn.UserID.String(),
		DatabaseID:    conn.DatabaseID.String(),
		SourceIP:      conn.SourceIP,
		ConnectedAt:   auditTimestamp(conn.ConnectedAt),
		InstanceID:    conn.InstanceID,
		RunID:         derefString(conn.RunID),
		GrantUID:      optionalUUIDString(conn.GrantUID),
		ClosedBy:      closedBy,
	}

	if conn.DisconnectedAt != nil {
		details.DisconnectedAt = auditTimestamp(*conn.DisconnectedAt)
	}

	// A session that logged nothing carries no stamp, and no writer invents one
	// — so the entry says nothing about a chain rather than claiming an empty
	// one.
	if conn.QueryChainMAC != nil {
		length := conn.QueryChainLen
		version := conn.QueryChainStampVersion

		details.QueryChainMAC = hex.EncodeToString(conn.QueryChainMAC)
		details.QueryChainLen = &length
		details.QueryChainStampVersion = &version
	}

	return connectionAuditEvent(AuditEventConnectionClosed, conn, details)
}

func connectionAuditEvent(eventType string, conn *Connection, details connectionAuditDetails) (*AuditEvent, error) {
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the session audit details: %w", err)
	}

	userID := conn.UserID

	return &AuditEvent{
		EventType: eventType,
		UserID:    &userID,
		Details:   encoded,
	}, nil
}

// recordConnectionOpened writes the session-open entry. Best effort by design —
// see writeConnectionAudit.
func (s *Store) recordConnectionOpened(ctx context.Context, conn *Connection) {
	event, err := connectionOpenedEvent(conn)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build the session audit entry",
			slog.String("connection", conn.UID.String()), slog.Any("error", err))

		return
	}

	s.writeConnectionAudit(ctx, event)
}

// recordConnectionClosed writes the session-close entry for one connection.
func (s *Store) recordConnectionClosed(ctx context.Context, conn *Connection, closedBy string) {
	event, err := connectionClosedEvent(conn, closedBy)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build the session audit entry",
			slog.String("connection", conn.UID.String()), slog.Any("error", err))

		return
	}

	s.writeConnectionAudit(ctx, event)
}

// recordReconciledCloses writes the session-close entries for a whole reconcile
// pass.
//
// The reconcile closes an arbitrary number of crash-orphaned connections in one
// transaction and stamps their chain heads in the next statement, so the rows
// are re-read here rather than returned by the close: the stamp does not exist
// yet when the UPDATE returns. That is one SELECT and one chained INSERT per
// batch of chainStampBatchSize, matching how the stamps themselves are written —
// never a round trip per connection.
func (s *Store) recordReconciledCloses(ctx context.Context, uids []uuid.UUID) {
	if len(uids) == 0 {
		return
	}

	for start := 0; start < len(uids); start += chainStampBatchSize {
		end := min(start+chainStampBatchSize, len(uids))

		rows, err := s.connectionAuditRows(ctx, uids[start:end])
		if err != nil {
			slog.ErrorContext(ctx, "failed to read the reconciled sessions to audit",
				slog.Int("connections", end-start), slog.Any("error", err))

			continue
		}

		events := make([]*AuditEvent, 0, len(rows))

		for i := range rows {
			event, err := connectionClosedEvent(&rows[i], connectionClosedByReconcile)
			if err != nil {
				slog.ErrorContext(ctx, "failed to build the session audit entry",
					slog.String("connection", rows[i].UID.String()), slog.Any("error", err))

				continue
			}

			events = append(events, event)
		}

		s.writeConnectionAudit(ctx, events...)
	}
}

// connectionAuditRows reads the identity of the given connections, stamp
// included, in one round trip.
func (s *Store) connectionAuditRows(ctx context.Context, uids []uuid.UUID) ([]Connection, error) {
	var rows []Connection

	err := s.db.NewSelect().
		Model(&rows).
		ColumnExpr(connectionAuditColumns).
		Where("uid IN (?)", bun.List(uids)).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read the connections to audit: %w", err)
	}

	return rows, nil
}

// writeConnectionAudit appends session audit entries, and never fails the
// session for it.
//
// Two deliberate choices, both of which the alternative gets wrong:
//
//   - **Not fatal.** A store that cannot write an audit entry must not take a
//     live database session down with it, so a failure is logged and the caller
//     carries on. The trade is that an entry can be *missing*; it can never be
//     fabricated, because every entry is written after the state it describes is
//     committed.
//
//   - **Outside the caller's transaction**, even where the caller has one. The
//     chain append owns its transaction: it takes the store-wide advisory lock
//     and retries on a stale head, and a colliding INSERT aborts the enclosing
//     transaction, so the retry has nowhere to run inside someone else's. Nesting
//     it would also invert the rule above — a lost audit entry would roll back a
//     completed close — and would hold every closed connection row's lock while
//     contending for the audit chain lock against every admin action in the
//     store. The cost is the gap: a process that dies between the close and this
//     write leaves a session whose close is unrecorded. That reads as a session
//     still open, which is what an *unclosed* row says too, so the two never
//     disagree in a way that accuses anyone.
//
// The context is detached from the caller's: a session close usually runs with a
// context that is already canceled — the client is gone — and an audit entry
// that only lands for sessions that ended politely is not an audit trail.
func (s *Store) writeConnectionAudit(ctx context.Context, events ...*AuditEvent) {
	if len(events) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectionAuditTimeout)
	defer cancel()

	if err := s.LogAuditEvents(ctx, events...); err != nil {
		slog.ErrorContext(ctx, "failed to record the session audit trail",
			slog.Int("events", len(events)), slog.Any("error", err))
	}
}

// auditTimestamp renders a timestamp the way the row stores it: UTC, truncated
// to the microsecond PostgreSQL keeps. Anything finer would put a value in the
// record that no read of the row can reproduce.
func auditTimestamp(t time.Time) string {
	return normalizeStoredTime(t).Format(time.RFC3339Nano)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

func optionalUUIDString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}

	return id.String()
}

// Termination describes a session dbbat ended itself: why, and — when a
// statement is what caused it — which statement and by how much it overran.
//
// It is protocol-agnostic on purpose. The statement-timeout watchdog is its
// first caller; the grant/quota/revocation teardowns and the admin
// terminate-connection endpoint are the next ones, and they all want the same
// three surfaces updated in the same order.
type Termination struct {
	// Reason is one of the Termination* constants. An empty Reason makes
	// RecordTermination a no-op: "dbbat did not end this session".
	Reason string

	// QueryUID is the statement that was in flight, uuid.Nil when none was.
	QueryUID uuid.UUID

	// Limit is the limit that was crossed and Observed how far past it the
	// session got. Both zero when the reason is not a duration.
	Limit    time.Duration
	Observed time.Duration

	// Detail is free text for the audit entry when the three fields above do
	// not say enough. Never shown to the client.
	Detail string

	// By is the username of the human who asked for this termination, empty
	// when no human did (every watchdog-driven reason). It is what makes the
	// audit entry and the in-flight statement's row name a person rather than
	// "dbbat".
	By string
}

// Set reports whether this record describes an actual dbbat-initiated
// termination, as opposed to the zero value every ordinary close carries.
func (t Termination) Set() bool {
	return t.Reason != ""
}

// Message is the human-readable one-liner a terminated session's in-flight
// query row carries, and the text the client is told where the protocol allows
// one. It names the limit and what was actually observed, because "your
// session was terminated" without either is unactionable.
func (t Termination) Message() string {
	switch {
	case !t.Set():
		return ""
	// A named human first: "session terminated by alice" is the whole answer to
	// "what happened to my connection?", and prefixing it with the vocabulary
	// value would only bury it.
	case t.By != "" && t.Detail != "":
		return "session terminated by " + t.By + ": " + t.Detail
	case t.By != "":
		return "session terminated by " + t.By
	case t.Reason == TerminationStatementTimeout && t.Limit > 0 && t.Observed > 0:
		return fmt.Sprintf("statement timeout: limit %s, ran %s, session terminated by dbbat",
			t.Limit, t.Observed.Round(100*time.Millisecond))
	case t.Reason == TerminationStatementTimeout && t.Limit > 0:
		return fmt.Sprintf("statement timeout: limit %s, session terminated by dbbat", t.Limit)
	case t.Detail != "":
		return t.Reason + ": " + t.Detail + ", session terminated by dbbat"
	default:
		return t.Reason + ": session terminated by dbbat"
	}
}

// recordConnectionTerminated writes the connection.terminated audit entry.
//
// It is written *before* the close entry rather than after, so a reader
// following the chain sees the reason and then the seal, which is the order the
// events actually happened in.
func (s *Store) recordConnectionTerminated(ctx context.Context, conn *Connection, t Termination) {
	if !t.Set() {
		return
	}

	details := connectionAuditDetails{
		ConnectionUID:      conn.UID.String(),
		UserID:             conn.UserID.String(),
		DatabaseID:         conn.DatabaseID.String(),
		SourceIP:           conn.SourceIP,
		ConnectedAt:        auditTimestamp(conn.ConnectedAt),
		InstanceID:         conn.InstanceID,
		RunID:              derefString(conn.RunID),
		GrantUID:           optionalUUIDString(conn.GrantUID),
		TerminationReason:  t.Reason,
		TerminatedBy:       t.By,
		TerminationDetails: t.Detail,
	}

	if conn.DisconnectedAt != nil {
		details.DisconnectedAt = auditTimestamp(*conn.DisconnectedAt)
	}

	if t.QueryUID != uuid.Nil {
		details.QueryUID = t.QueryUID.String()
	}

	if t.Limit > 0 {
		details.LimitSeconds = t.Limit.String()
	}

	if t.Observed > 0 {
		details.ObservedDuration = t.Observed.Round(time.Millisecond).String()
	}

	event, err := connectionAuditEvent(AuditEventConnectionTerminated, conn, details)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build the session termination audit entry",
			slog.String("connection", conn.UID.String()), slog.Any("error", err))

		return
	}

	s.writeConnectionAudit(ctx, event)
}
