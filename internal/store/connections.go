package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// CreateConnection creates a new connection record
func (s *Store) CreateConnection(ctx context.Context, userID, databaseID uuid.UUID, sourceIP string) (*Connection, error) {
	conn := &Connection{
		UID:              newUIDv7(), // Generate UUIDv7 for time-ordered inserts
		UserID:           userID,
		DatabaseID:       databaseID,
		SourceIP:         sourceIP,
		ConnectedAt:      time.Now(),
		LastActivityAt:   time.Now(),
		Queries:          0,
		BytesTransferred: 0,
		InstanceID:       s.instanceID,
	}

	_, err := s.db.NewInsert().
		Model(conn).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection: %w", err)
	}

	return conn, nil
}

// CloseConnection sets the disconnected_at timestamp
func (s *Store) CloseConnection(ctx context.Context, uid uuid.UUID) error {
	now := time.Now()
	result, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Where("disconnected_at IS NULL").
		Set("disconnected_at = ?", now).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to close connection: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrConnectionNotFound
	}

	return nil
}

// OrphanedConnections counts what one startup reconcile closed, split by whose
// rows they were. The two numbers mean very different things operationally:
// Own is this instance's own previous run not shutting down cleanly, Reclaimed
// is *another* process having died without shutting down at all.
type OrphanedConnections struct {
	// Own is the number of connections this instance id left open itself.
	Own int64

	// Reclaimed is the number of connections closed on behalf of instances that
	// are provably gone — deregistered, or past InstanceStaleAfter.
	Reclaimed int64
}

// Total is the number of connection rows the reconcile closed.
func (o OrphanedConnections) Total() int64 {
	return o.Own + o.Reclaimed
}

// CloseOrphanedConnections stamps disconnected_at on every connection left open
// by a process that is no longer running — this instance's own previous run,
// plus any other instance the registry proves is gone — and reports the two
// counts separately. Call it once at startup, before the proxies begin
// accepting, and after RegisterInstance.
//
// Why it is needed: disconnected_at is otherwise only ever written by
// CloseConnection, on a clean session teardown. A crash, a kill or a pod
// reschedule skips that, so those rows keep disconnected_at NULL forever. The
// retention sweep (CleanupOldQueryRows) only reaps connections with
// disconnected_at IS NOT NULL — deleting a row a live session still logs
// against would break the foreign key — so an orphan survives every sweep,
// outlives all of its queries, and keeps counting as "currently connected".
//
// Why it is not a blanket UPDATE ... WHERE disconnected_at IS NULL: dbbat is
// deployed with more than one replica against a shared store (see
// docs/approvals.md, "Multiple replicas", and charts/dbbat/values.yaml). A
// blanket update would let a starting replica mark another replica's *live*
// connections as disconnected. That is not cosmetic — those rows would
// immediately satisfy the retention sweep's cutoff predicate, so the sweep
// could delete a connection a live session is still writing queries against.
//
// The test that replaces the blanket update is liveness, not identity. A
// running process upserts a row in `instances` at startup and refreshes it
// every InstanceHeartbeatInterval; a clean shutdown deletes it. So another
// instance's connections are only touched when that instance has no row at all
// (it shut down cleanly, or never registered) or has not been seen for
// InstanceStaleAfter — 30 missed heartbeats. A live replica is therefore never
// a candidate: it would have to fail every heartbeat for a quarter of an hour
// while still serving traffic.
//
// Legacy rows carrying an empty instance id — created before the instance_id
// column existed — are folded into the "no instances row" case rather than
// being given a separate opt-in switch. That is a deliberate choice, and it is
// safe because the empty id can never be alive: config.resolveInstanceID
// guarantees a non-empty id for any serving process (hostname, else the
// FallbackInstanceID constant), a store with an empty instance id refuses to
// register or reconcile at all, and RegisterInstance refuses to write a row for
// it — so nothing can ever make it look fresh. The one moment such rows could
// have belonged to a live session is the upgrade that introduced the column,
// and the 20260803030000_instances migration covers it by seeding the registry
// (empty id included) from the open connections, which buys every one of those
// owners a full grace period.
//
// A store whose own instance id is empty is refused outright (zero, nil).
// Reconciling would then treat the empty id as this process's identity, which
// is exactly the blanket update this design exists to prevent.
func (s *Store) CloseOrphanedConnections(ctx context.Context) (OrphanedConnections, error) {
	var counts OrphanedConnections

	if s.instanceID == "" {
		return counts, nil
	}

	own, err := s.closeOrphans(ctx, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("instance_id = ?", s.instanceID)
	})
	if err != nil {
		return counts, err
	}

	counts.Own = own

	staleBefore := time.Now().Add(-InstanceStaleAfter)

	reclaimed, err := s.closeOrphans(ctx, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		// Own rows are handled above; excluding them here keeps the two counts
		// disjoint (and this instance is registered, so it would not match).
		return q.Where("instance_id <> ?", s.instanceID).
			Where(`NOT EXISTS (
				SELECT 1 FROM instances AS live
				WHERE live.instance_id = c.instance_id
				  AND live.last_seen_at >= ?
			)`, staleBefore)
	})
	if err != nil {
		return counts, err
	}

	counts.Reclaimed = reclaimed

	return counts, nil
}

// closeOrphans runs one scoped reconcile and returns how many rows it closed.
func (s *Store) closeOrphans(ctx context.Context, scope func(*bun.UpdateQuery) *bun.UpdateQuery) (int64, error) {
	q := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("disconnected_at IS NULL").
		// last_activity_at, not now(): retention should measure from when the
		// session actually stopped talking, and a crashed session must not get
		// its clock reset by every subsequent restart.
		Set("disconnected_at = last_activity_at")

	result, err := scope(q).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to close orphaned connections: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rowsAffected, nil
}

// UpdateConnectionActivity updates the last_activity_at timestamp
func (s *Store) UpdateConnectionActivity(ctx context.Context, uid uuid.UUID) error {
	_, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Set("last_activity_at = ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update connection activity: %w", err)
	}
	return nil
}

// IncrementConnectionStats increments the query count by 1 and adds bytes to bytes_transferred
func (s *Store) IncrementConnectionStats(ctx context.Context, uid uuid.UUID, bytes int64) error {
	_, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Set("queries = queries + 1").
		Set("bytes_transferred = bytes_transferred + ?", bytes).
		Set("last_activity_at = ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to increment connection stats: %w", err)
	}
	return nil
}

// IncrementConnectionBytes adds bytes to bytes_transferred WITHOUT bumping the
// query count. Used to flush client-side bytes that are not attributable to a
// completed query log row — e.g. a query aborted mid-stream by a grant limit
// (whose response never reached the normal completion path) or the trailing
// response bytes of the last query, written after per-query bookkeeping ran.
// Persisting them keeps the grant's recomputed bytes_transferred honest across
// reconnects instead of undercounting.
func (s *Store) IncrementConnectionBytes(ctx context.Context, uid uuid.UUID, bytes int64) error {
	_, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Set("bytes_transferred = bytes_transferred + ?", bytes).
		Set("last_activity_at = ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to increment connection bytes: %w", err)
	}
	return nil
}

// GetConnectionByUID retrieves a single connection by UID
func (s *Store) GetConnectionByUID(ctx context.Context, uid uuid.UUID) (*Connection, error) {
	conn := &Connection{}
	err := s.db.NewSelect().
		Model(conn).
		ColumnExpr("uid, user_id, database_id, source_ip::text, connected_at, last_activity_at, disconnected_at, queries, bytes_transferred, instance_id").
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConnectionNotFound
		}
		return nil, fmt.Errorf("failed to get connection: %w", err)
	}
	return conn, nil
}

// ListConnections retrieves connections with optional filters
func (s *Store) ListConnections(ctx context.Context, filter ConnectionFilter) ([]Connection, error) {
	var connections []Connection
	q := s.db.NewSelect().
		Model(&connections).
		ColumnExpr("uid, user_id, database_id, source_ip::text, connected_at, last_activity_at, disconnected_at, queries, bytes_transferred, instance_id")

	if filter.UserID != nil {
		q = q.Where("user_id = ?", *filter.UserID)
	}

	if filter.DatabaseID != nil {
		q = q.Where("database_id = ?", *filter.DatabaseID)
	}

	if filter.BeforeUID != nil {
		q = q.Where("uid < ?", *filter.BeforeUID)
	}

	q = q.Order("uid DESC")

	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}

	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}

	err := q.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list connections: %w", err)
	}

	if connections == nil {
		connections = []Connection{}
	}
	return connections, nil
}
