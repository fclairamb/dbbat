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
)

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
var SessionAuditEventTypes = []string{AuditEventConnectionOpened, AuditEventConnectionClosed}

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
	DisconnectedAt         string `json:"disconnected_at,omitempty"`
	ClosedBy               string `json:"closed_by,omitempty"`
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
	"disconnected_at, instance_id, run_id, grant_uid, " +
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
