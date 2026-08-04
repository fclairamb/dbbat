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
// The two halves have different lifetimes, which is why they are separate
// methods. The own half is startup-only by nature: while this process runs, a
// row carrying its instance id and no disconnected_at is a session it is
// serving right now. The reclaim half — ReclaimDeadInstanceConnections — is
// liveness-checked rather than identity-scoped, so it is safe at any time and
// is also run periodically (see InstanceReclaimInterval).
//
// A store whose own instance id is empty is refused outright (zero, nil).
// Reconciling would then treat the empty id as this process's identity, which
// is exactly the blanket update this design exists to prevent.
func (s *Store) CloseOrphanedConnections(ctx context.Context) (OrphanedConnections, error) {
	var counts OrphanedConnections

	if s.instanceID == "" {
		return counts, nil
	}

	own, err := s.closeOwnOrphanedConnections(ctx)
	if err != nil {
		return counts, err
	}

	counts.Own = own

	reclaimed, err := s.ReclaimDeadInstanceConnections(ctx)
	if err != nil {
		return counts, err
	}

	counts.Reclaimed = reclaimed

	return counts, nil
}

// closeOwnOrphanedConnections closes the still-open connections carrying this
// process's own instance id, on the assumption that they belong to its previous
// run.
//
// Startup-only, and unexported for that reason: it is the one branch with no
// liveness test — "my id" is taken to mean "my previous run", which is only
// true before this process has accepted anything. Calling it once the proxies
// are up would close this run's own live sessions.
func (s *Store) closeOwnOrphanedConnections(ctx context.Context) (int64, error) {
	if s.instanceID == "" {
		return 0, nil
	}

	return s.closeOrphans(ctx, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("instance_id = ?", s.instanceID)
	})
}

// ReclaimDeadInstanceConnections closes the connections of *other* instances
// the registry proves are gone, and returns how many it closed.
//
// Unlike the own-instance half of CloseOrphanedConnections this is not tied to
// startup: it decides on liveness, not on identity, so it holds at any point in
// the process's life. It runs both from the startup reconcile and on a timer
// (InstanceReclaimInterval), because the crash case is otherwise only ever
// noticed by an unrelated restart: a SIGKILLed pod leaves a registry row whose
// last_seen_at is seconds old, so its replacement — starting immediately —
// reclaims nothing, and by the time the row does go stale nothing is starting
// any more.
//
// The test is liveness, not identity. A running process upserts a row in
// `instances` at startup and refreshes it every InstanceHeartbeatInterval; a
// clean shutdown deletes it. So another instance's connections are only touched
// when that instance has no row at all (it shut down cleanly, or never
// registered) or has not been seen for InstanceStaleAfter — 30 missed
// heartbeats. A live replica is therefore never a candidate: it would have to
// fail every heartbeat for a quarter of an hour while still serving traffic.
//
// Legacy rows carrying an empty instance id — created before the instance_id
// column existed — are folded into the "no instances row" case rather than
// being given a separate opt-in switch. That is a deliberate choice, and it is
// safe because the empty id can never be alive: config.resolveInstanceID
// guarantees a non-empty id for any serving process (hostname, else the
// FallbackInstanceID constant), a store with an empty instance id refuses to
// register or reconcile at all, and RegisterInstance refuses to write a row for
// it — so nothing can ever make it look fresh.
//
// The one moment such rows could have belonged to a live session is the upgrade
// that introduces this liveness tracking, since no replica on the previous
// build can register itself. The 20260803030000_instances migration covers that
// by seeding the registry — the empty id included — from every instance id the
// connections table has recorded, which buys each of those owners a full grace
// period. The coverage is not total, and cannot be: an old-build replica that
// has never recorded a connection is not seeded, so if it accepts its first
// session between the migration and the next process start, that session is
// reclaimed through the no-registry-row branch, which by design has no grace
// period at all (a deleted row means a clean shutdown, and reclaiming it
// immediately is the point). Giving that branch a grace period would trade a
// window that lasts one upgrade, and only for a replica that has served nothing
// since the retention horizon, against permanently delaying the case this
// feature is built for. The window is left open knowingly.
//
// A store whose own instance id is empty reclaims nothing (zero, nil), matching
// CloseOrphanedConnections: a process with no identity of its own has no
// business judging anyone else's.
func (s *Store) ReclaimDeadInstanceConnections(ctx context.Context) (int64, error) {
	if s.instanceID == "" {
		return 0, nil
	}

	return s.closeOrphans(ctx, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		// Our own rows are excluded: at startup they are the other half's job,
		// which keeps the two counts disjoint, and on the periodic pass they
		// are this run's live sessions, which must never be touched. Either
		// way we are registered and heartbeating, so the liveness clause below
		// would spare them anyway — this is belt and braces.
		return q.Where("instance_id <> ?", s.instanceID).
			Where(noLiveOwner())
	})
}

// noLiveOwner matches connection rows whose owning instance is not alive:
// either it has no registry row at all (clean shutdown, never registered, or
// the legacy empty id) or its last heartbeat predates the grace period.
//
// The staleness cutoff is computed by the database, not by this process: the
// heartbeat it is being compared against was written by another replica, and
// clock skew between the two would come straight out of the grace period. See
// instanceNow.
//
// `c` is the alias bun gives the connections table in an UPDATE.
func noLiveOwner() string {
	return `NOT EXISTS (
		SELECT 1 FROM instances AS live
		WHERE live.instance_id = c.instance_id
		  AND live.last_seen_at >= ` + instanceStaleCutoff() + `
	)`
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
