package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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

// CloseOrphanedConnections stamps disconnected_at on every connection this
// instance left open, and returns how many rows it reconciled. Call it once at
// startup, before the proxies begin accepting.
//
// Why it is needed: disconnected_at is otherwise only ever written by
// CloseConnection, on a clean session teardown. A crash, a kill or a pod
// reschedule skips that, so those rows keep disconnected_at NULL forever. The
// retention sweep (CleanupOldQueryRows) only reaps connections with
// disconnected_at IS NOT NULL — deleting a row a live session still logs
// against would break the foreign key — so an orphan survives every sweep,
// outlives all of its queries, and keeps counting as "currently connected".
//
// Why it is scoped rather than a blanket UPDATE ... WHERE disconnected_at IS
// NULL: dbbat is deployed with more than one replica against a shared store
// (see docs/approvals.md, "Multiple replicas", and charts/dbbat/values.yaml).
// A blanket update would let a starting replica mark another replica's *live*
// connections as disconnected. That is not cosmetic — those rows would
// immediately satisfy the retention sweep's cutoff predicate, so the sweep
// could delete a connection a live session is still writing queries against.
// Scoping by instance id makes that impossible: an instance only ever closes
// rows it opened itself.
//
// What this does NOT reclaim: rows owned by an instance id that never comes
// back. The default instance id is the hostname, so a single host, a
// StatefulSet, or an explicit DBB_INSTANCE_ID all reclaim their own orphans on
// restart — but a plain Kubernetes Deployment mints a new pod name every time,
// so a replacement pod does not recognize its predecessor's rows. Connections
// created before the instance_id column existed carry ” and are likewise never
// reclaimed. Clearing that residue needs instance liveness tracking rather than
// identity alone; see specs/todos/2026-08-03-reclaim-dead-instance-connections.md.
//
// An empty instance id is refused (0, nil): stamping and reconciling would then
// both key off ”, which is the blanket update this scoping exists to prevent.
func (s *Store) CloseOrphanedConnections(ctx context.Context) (int64, error) {
	if s.instanceID == "" {
		return 0, nil
	}

	result, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("instance_id = ?", s.instanceID).
		Where("disconnected_at IS NULL").
		// last_activity_at, not now(): retention should measure from when the
		// session actually stopped talking, and a crashed session must not get
		// its clock reset by every subsequent restart.
		Set("disconnected_at = last_activity_at").
		Exec(ctx)
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
