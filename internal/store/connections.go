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

// ConnectionOption sets a field on a connection row before it is inserted.
// Variadic so the many call sites that only need the four required columns stay
// as they are.
type ConnectionOption func(*Connection)

// WithUpstreamTLS records whether the proxy→upstream leg of the session is
// encrypted. Only the session knows: under an opportunistic ssl_mode the row's
// policy does not determine the outcome.
func WithUpstreamTLS(encrypted bool) ConnectionOption {
	return func(c *Connection) { c.UpstreamTLS = encrypted }
}

// WithGrantUID stamps the connection with the grant it authenticated under.
// Every proxy holds the grant returned by its auth-time GetActiveGrant (or
// equivalent) lookup right where it creates the connection row — this is
// simply that pick, pinned for the row's whole life. See Connection.GrantUID.
func WithGrantUID(grantUID uuid.UUID) ConnectionOption {
	return func(c *Connection) { c.GrantUID = &grantUID }
}

// CreateConnection creates a new connection record
func (s *Store) CreateConnection(
	ctx context.Context,
	userID, databaseID uuid.UUID,
	sourceIP string,
	opts ...ConnectionOption,
) (*Connection, error) {
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
		RunID:            s.currentRunID(),
	}

	for _, opt := range opts {
		opt(conn)
	}

	_, err := s.db.NewInsert().
		Model(conn).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection: %w", err)
	}

	return conn, nil
}

// CloseConnection sets the disconnected_at timestamp and, when chaining is on,
// stamps the final head of this connection's query chain onto the row.
//
// The stamped head is what makes a *trailing* deletion detectable: without it,
// removing the last statements of a session would leave a shorter chain that
// still verified end to end. With it, the surviving rows no longer compute the
// head the connection claims.
func (s *Store) CloseConnection(ctx context.Context, uid uuid.UUID) error {
	now := time.Now()

	q := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Where("disconnected_at IS NULL").
		Set("disconnected_at = ?", now)

	if s.ChainEnabled() {
		seq, mac, err := s.queryChainHead(ctx, uid)
		if err != nil {
			return err
		}

		if mac != nil {
			q = q.Set("query_chain_mac = ?", mac).Set("query_chain_len = ?", seq)
		}
	}

	result, err := q.Exec(ctx)

	// Whatever happened to the row, this process is done writing to that
	// chain: keeping the cached head would leak an entry per session.
	s.queryChains.forget(uid)

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

// queryChainHead returns the head this process holds for a connection's query
// chain, falling back to the database when this process never wrote to it (a
// replica closing a session it did not open, or a store restarted mid-session).
func (s *Store) queryChainHead(ctx context.Context, connectionUID uuid.UUID) (int64, []byte, error) {
	state := s.queryChains.get(connectionUID)

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.loaded {
		return state.seq, state.mac, nil
	}

	return readQueryChainHead(ctx, s.db, connectionUID)
}

// OrphanedConnections counts what one startup reconcile closed, split by whose
// rows they were. The two numbers mean very different things operationally:
// Own is a previous run of this instance id not shutting down cleanly,
// Reclaimed is *another* process having died without shutting down at all.
type OrphanedConnections struct {
	// Own is the number of connections a previous run carrying this instance id
	// left open — never this run's own, and never a live peer's, however the
	// id came to be shared.
	Own int64

	// Reclaimed is the number of connections closed on behalf of runs that are
	// provably gone — deregistered, or past InstanceStaleAfter.
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
// Why an instance id is not enough to scope it: an id is unique per live
// process only by convention — an operator can pin DBB_INSTANCE_ID to the same
// value on every replica, and config.FallbackInstanceID is where every replica
// that cannot read its hostname lands. Both halves therefore key on the run
// id, minted in memory at startup and unshareable, and both then ask the same
// question of the registry: does a live run still own this row? Identity only
// decides which of the two counts a row lands in.
//
// The two halves are separate methods because they are reported separately and
// because only one of them belongs at startup. The own half is the previous
// runs of this instance id; the reclaim half — ReclaimDeadInstanceConnections
// — is everyone else, and is also run periodically (see
// InstanceReclaimInterval), which is what eventually picks up a run that was
// still inside its grace period when we started.
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
// process's own instance id but not its run id — its previous runs' leftovers,
// as far as the registry can prove they are over.
//
// "My id, not my run" is the whole fix. An instance id is only unique per live
// process by convention: an operator can pin the same DBB_INSTANCE_ID on every
// replica, and config.FallbackInstanceID is reached by every replica that
// cannot read its hostname. This branch used to close every open row carrying
// its id with no liveness test at all, so under a shared id a starting replica
// closed a live peer's sessions — rows that then satisfy the retention sweep's
// cutoff while a session is still writing queries against them. The run id is
// minted in memory and cannot be shared, so scoping by "not my run" and then
// applying the same liveness test as the reclaim branch makes the two branches
// uniform: a row is closed only when no live run owns it.
//
// The cost is that a crashed previous run is no longer reclaimed the instant we
// restart: its registry row is seconds old, so it still looks alive, and the
// rows wait for the grace period like any other dead run's. Nothing leaks —
// ReclaimDeadInstanceConnections covers our own instance id's other runs too,
// and runs on a timer — the reclaim is just no longer instantaneous. That is
// the price of not trusting an id to mean a process, and the case it buys is
// the one where trusting it destroys live data.
//
// It stays startup-only and unexported all the same: mid-life it would be
// redundant (the periodic reclaim matches a superset of these rows), and
// keeping it at startup is what keeps the two reported counts meaning "my own
// previous run" and "somebody else".
func (s *Store) closeOwnOrphanedConnections(ctx context.Context) (int64, error) {
	if s.instanceID == "" {
		return 0, nil
	}

	return s.closeOrphans(ctx, s.ownOrphanScope)
}

// ownOrphanScope narrows the reconcile to rows carrying this instance id but
// not this run id, that no live run owns.
func (s *Store) ownOrphanScope(q *bun.UpdateQuery) *bun.UpdateQuery {
	// IS DISTINCT FROM, not <>: a NULL run_id — a row opened before run
	// tracking existed — is one of ours to consider, and plain inequality
	// would drop it.
	return q.Where("instance_id = ?", s.instanceID).
		Where("run_id IS DISTINCT FROM ?", s.runID).
		Where(noLiveOwner(), s.instanceID, s.runID)
}

// ReclaimDeadInstanceConnections closes the connections of every run other than
// this one that the registry proves is gone, and returns how many it closed.
//
// Unlike the own half of CloseOrphanedConnections this is not tied to startup:
// everything except this run is in scope, so it holds at any point in the
// process's life. It runs both from the startup reconcile and on a timer
// (InstanceReclaimInterval), because the crash case is otherwise only ever
// noticed by an unrelated restart: a SIGKILLed pod leaves a registry row whose
// last_seen_at is seconds old, so its replacement — starting immediately —
// reclaims nothing, and by the time the row does go stale nothing is starting
// any more. Since a restart mints a fresh run id, that now covers our own
// predecessor as well as other instances: a stable instance id no longer means
// its crashed run's rows wait for a restart that may be days away.
//
// The test is liveness, not identity. A running process upserts a row in
// `instances` for its (instance id, run id) at startup and refreshes it every
// InstanceHeartbeatInterval; a clean shutdown deletes it. So another run's
// connections are only touched when that run has no row at all (it shut down
// cleanly, or never registered) or has not been seen for InstanceStaleAfter —
// 30 missed heartbeats. A live replica is therefore never a candidate, even one
// sharing our instance id: it would have to fail every heartbeat for a quarter
// of an hour while still serving traffic.
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
// period. The same reasoning, and the same remedy, apply to the run id one
// migration later: rows written before it existed carry NULL, are judged by
// their instance id alone (see noLiveOwner), and 20260804120000 refreshes the
// registry rows of every pre-run-tracking owner so the upgrade window is a full
// grace period rather than an instant.
//
// The coverage is not total, and cannot be: an old-build replica that
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

	return s.closeOrphans(ctx, s.deadRunScope)
}

// deadRunScope narrows the reconcile to every run except this one that the
// registry proves is gone.
func (s *Store) deadRunScope(q *bun.UpdateQuery) *bun.UpdateQuery {
	// The exclusion is this *run*, not this instance id. Our own live
	// sessions must never be touched — on the periodic pass they are what
	// this process is serving right now — but another run carrying our id
	// is somebody else's, whether it is our own crashed predecessor or a
	// replica that was handed the same DBB_INSTANCE_ID. Those are judged
	// on liveness like everyone else's, which is also what stops a
	// predecessor's rows from lingering until the next restart when the id
	// is stable (a StatefulSet, or a pinned id).
	//
	// At startup this scope is a superset of the own branch's, and that is
	// harmless: the own branch runs first and takes its rows, so the two
	// counts stay disjoint.
	//
	// IS DISTINCT FROM, not <>: rows predating run tracking carry NULL.
	return q.Where("instance_id <> ? OR run_id IS DISTINCT FROM ?", s.instanceID, s.runID).
		Where(noLiveOwner(), s.instanceID, s.runID)
}

// noLiveOwner matches connection rows that no live run owns: either the owning
// run has no registry row at all (clean shutdown, never registered, or the
// legacy empty instance id) or its last heartbeat predates the grace period.
//
// Ownership is matched on the pair, so a fresh row proves only that *that run*
// is alive. Matching on the instance id alone would be both too generous and
// too strict: too generous because our own brand-new registration would vouch
// for every row our id has ever opened, and too strict because a replica that
// shares an id with a live peer could never be told apart from it.
//
// Rows whose run_id is NULL predate run tracking, and their owner is a build
// that only maintains the per-id registry — so they are judged by the rule
// their owner is playing by: any live run of that instance id keeps them. That
// is the pre-existing behavior, kept deliberately rather than folded into the
// stricter test, which would have declared every one of them ownerless the
// moment this migration landed and closed sessions that were still being
// served through the upgrade.
//
// Our own registry row never vouches for anything: it is excluded outright.
// For run-matched rows that changes nothing (a row carrying our run id is out
// of scope in both branches), but it is what stops the NULL fallback above
// from turning our fresh registration into a permanent shield over the rows
// our *previous* runs left behind — the leak this whole mechanism exists to
// close.
//
// The staleness cutoff is computed by the database, not by this process: the
// heartbeat it is being compared against was written by another replica, and
// clock skew between the two would come straight out of the grace period. See
// instanceNow.
//
// `c` is the alias bun gives the connections table in an UPDATE. The two
// placeholders are this process's instance id and run id, in that order.
func noLiveOwner() string {
	return `NOT EXISTS (
		SELECT 1 FROM instances AS live
		WHERE live.instance_id = c.instance_id
		  AND (c.run_id IS NULL OR live.run_id = c.run_id)
		  AND (live.instance_id <> ? OR live.run_id <> ?)
		  AND live.last_seen_at >= ` + instanceStaleCutoff() + `
	)`
}

// closeOrphans runs one scoped reconcile and returns how many rows it closed.
func (s *Store) closeOrphans(ctx context.Context, scope func(*bun.UpdateQuery) *bun.UpdateQuery) (int64, error) {
	result, err := scope(s.orphanCloseQuery()).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to close orphaned connections: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return rowsAffected, nil
}

// orphanCloseQuery builds the reconcile's UPDATE, minus the scope predicate its
// caller adds. Split out from closeOrphans so a test can EXPLAIN the real
// statement instead of a hand-written lookalike.
//
// It stamps the query chain head as well as disconnected_at, in the *same*
// statement, so a crash-orphaned session ends up as tamper-evident at its tail
// as one CloseConnection closed. The head is recoverable without the process
// that died — it is the highest chain_seq on the connection and its MAC, which
// is exactly queryChainHeadSelect — so the reconcile does not need the in-memory
// chain state that died with it.
//
// Two caveats, both deliberate:
//
//   - The stamp seals whatever survived *at reconcile time*, not what the
//     session actually wrote. Someone who deleted trailing statements between
//     the crash and the reconcile gets the truncated chain blessed. That is
//     strictly better than no stamp at all — from the reconcile onward the tail
//     is protected — and it is the reason the stamp goes in this statement
//     rather than a later pass: the window is the reconcile's own UPDATE, not
//     the gap between two of them.
//   - The subquery copies `mac` verbatim, which is the format CloseConnection
//     writes and therefore the one to stay consistent with today. It is also the
//     weakness that
//     specs/todos/2026-08-10-06-seal-the-connection-query-chain-stamp.md exists to
//     fix: a verbatim head is readable out of `queries`, so an attacker can
//     recopy it after a trailing deletion. **When that spec lands, this path has
//     to be reworked** — a keyed stamp is HMAC(chain key, …) and the chain key
//     lives only in this process, so it cannot be computed in SQL. The reconcile
//     stops being one pure-SQL UPDATE: it has to select the orphans' heads, seal
//     each in Go, and write them back (in the same transaction as the close, to
//     keep the window above from widening).
//
// Cost: PostgreSQL evaluates a SET expression only for rows that pass the
// WHERE, so this adds two `idx_queries_chain_seq` lookups (one per column) per
// *closed* row — not per connection in the table, which is what would have made
// the reconcile scale with the store. Folding the two into one LEFT JOIN LATERAL
// was considered and dropped: it halves an already negligible cost and turns the
// "logged nothing" case into a join that has to be kept from filtering the row
// out. See TestQueryChainOrphanStampCostScalesWithOrphans, which asserts the
// loop count against a real plan.
func (s *Store) orphanCloseQuery() *bun.UpdateQuery {
	q := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("disconnected_at IS NULL").
		// last_activity_at, not now(): retention should measure from when the
		// session actually stopped talking, and a crashed session must not get
		// its clock reset by every subsequent restart.
		Set("disconnected_at = last_activity_at")

	if !s.ChainEnabled() {
		return q
	}

	// Correlated against the row being updated; `c` is bun's alias for
	// connections, `q` its alias for queries.
	headMAC := queryChainHeadSelect(s.db, "mac").Where("q.connection_id = c.uid")
	headLen := queryChainHeadSelect(s.db, "chain_seq").Where("q.connection_id = c.uid")

	// A connection that logged nothing keeps a NULL mac — there is no head to
	// seal, and a NULL stamp is never itself a break (checkStampedHead skips
	// it). query_chain_len is NOT NULL in the schema, so its absence is 0.
	return q.
		Set("query_chain_mac = (?)", headMAC).
		Set("query_chain_len = COALESCE((?), 0)", headLen)
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

// SetConnectionDumpKey records where this connection's session capture was
// uploaded. Called by the capture uploader after the object is in place and
// before the local spool copy is removed, so a capture is always addressable in
// exactly one place.
//
// A connection row that has since been reaped by the retention sweep is not an
// error: the capture outliving its row is a retention-ordering artifact, not a
// failed upload, and failing here would make the uploader retry forever.
func (s *Store) SetConnectionDumpKey(ctx context.Context, uid uuid.UUID, key string) error {
	_, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", uid).
		Set("dump_key = ?", key).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set connection dump key: %w", err)
	}

	return nil
}

// ClearConnectionDumpKey forgets the uploaded capture of a connection, after
// the object itself has been deleted.
func (s *Store) ClearConnectionDumpKey(ctx context.Context, uid uuid.UUID) error {
	return s.SetConnectionDumpKey(ctx, uid, "")
}

// GetConnectionByUID retrieves a single connection by UID
func (s *Store) GetConnectionByUID(ctx context.Context, uid uuid.UUID) (*Connection, error) {
	conn := &Connection{}
	err := s.db.NewSelect().
		Model(conn).
		ColumnExpr("uid, user_id, database_id, source_ip::text, connected_at, last_activity_at, "+
			"disconnected_at, queries, bytes_transferred, instance_id, upstream_tls, dump_key, grant_uid, "+
			"query_chain_mac, query_chain_len").
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
		ColumnExpr("uid, user_id, database_id, source_ip::text, connected_at, last_activity_at, " +
			"disconnected_at, queries, bytes_transferred, instance_id, upstream_tls, dump_key, grant_uid")

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
