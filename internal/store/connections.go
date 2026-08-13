package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	// Truncated to what timestamptz can store: connected_at is copied into the
	// session's audit entry, and a value the row cannot reproduce would make the
	// two disagree on read.
	now := normalizeStoredTime(time.Now())

	conn := &Connection{
		UID:              newUIDv7(), // Generate UUIDv7 for time-ordered inserts
		UserID:           userID,
		DatabaseID:       databaseID,
		SourceIP:         sourceIP,
		ConnectedAt:      now,
		LastActivityAt:   now,
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

	// After the row exists, so the entry can only ever be missing, never
	// describe a session that was not opened. Never fatal to the session — see
	// writeConnectionAudit.
	s.recordConnectionOpened(ctx, conn)

	return conn, nil
}

// CloseConnection sets the disconnected_at timestamp and, when chaining is on,
// stamps the final head of this connection's query chain onto the row.
//
// The stamped head is what makes a *trailing* deletion detectable: without it,
// removing the last statements of a session would leave a shorter chain that
// still verified end to end. With it, the surviving rows no longer compute the
// head the connection claims.
//
// The stamp is keyed (queryChainStampMAC), not a copy of the head MAC. A
// verbatim head is readable out of `queries`, so an attacker who deleted the
// tail of a session could simply recopy it; sealing means correcting the stamp
// after a deletion needs the chain key, exactly like forging a statement.
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
			// Never move the stamp backwards — see stampChainHeadBatch for why
			// a regression can only mean a trailing deletion. It matters here
			// too because a live session may already carry a refresh stamp: seq
			// is this process's own head, authoritative when it served the
			// session but read back from the database when it did not (a
			// replica closing a session it did not open, a store restarted
			// mid-session). Every SET expression sees the pre-update row, so
			// the three stay consistent.
			q = q.
				Set("query_chain_mac = CASE WHEN ?::bigint >= query_chain_len THEN ?::bytea "+
					"ELSE query_chain_mac END",
					seq, s.queryChainStampMAC(uid, queryChainStampKeyed, seq, mac)).
				Set("query_chain_stamp_version = CASE WHEN ?::bigint >= query_chain_len THEN ?::smallint "+
					"ELSE query_chain_stamp_version END", seq, queryChainStampKeyed).
				Set("query_chain_len = GREATEST(query_chain_len, ?::bigint)", seq)
		}
	}

	// RETURNING rather than a second SELECT: the session audit entry is built
	// from the row as the close left it — disconnected_at and the stamp above —
	// and the guard on disconnected_at is also what makes the returned rows the
	// answer to "did this call close anything?". A second call closes nothing,
	// returns nothing and therefore writes no second entry.
	var closed []Connection

	err := q.Returning(connectionAuditColumns).Scan(ctx, &closed)

	// Whatever happened to the row, this process is done writing to that
	// chain: keeping the cached head would leak an entry per session.
	s.queryChains.forget(uid)

	if err != nil {
		return fmt.Errorf("failed to close connection: %w", err)
	}

	if len(closed) == 0 {
		return ErrConnectionNotFound
	}

	s.recordConnectionClosed(ctx, &closed[0], connectionClosedBySession)

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
//
// With chaining on it is three statements in one transaction rather than one:
// the close returns the uids it took, their chain heads are read back, and the
// sealed stamps are written. The transaction is not optional — see
// stampOrphanHeads for what a second window would cost.
//
// Both paths return the uids they closed, because each of those sessions owes
// `audit_log` a connection.closed entry. That is written after the transaction
// commits and in batches (recordReconciledCloses), never inside it and never one
// connection at a time — see writeConnectionAudit for why the audit write must
// not be able to roll a completed reconcile back.
func (s *Store) closeOrphans(ctx context.Context, scope func(*bun.UpdateQuery) *bun.UpdateQuery) (int64, error) {
	var uids []uuid.UUID

	if !s.ChainEnabled() {
		if err := scope(s.orphanCloseQuery(s.db)).Returning("uid").Scan(ctx, &uids); err != nil {
			return 0, fmt.Errorf("failed to close orphaned connections: %w", err)
		}

		s.recordReconciledCloses(ctx, uids)

		return int64(len(uids)), nil
	}

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Reset per attempt: RunInTx does not retry, but a partially filled
		// slice from a failed statement must not survive into the audit pass.
		uids = nil

		if err := scope(s.orphanCloseQuery(tx)).Returning("uid").Scan(ctx, &uids); err != nil {
			return fmt.Errorf("failed to close orphaned connections: %w", err)
		}

		if len(uids) == 0 {
			return nil
		}

		return s.stampOrphanHeads(ctx, tx, uids)
	})
	if err != nil {
		return 0, err
	}

	s.recordReconciledCloses(ctx, uids)

	return int64(len(uids)), nil
}

// orphanCloseQuery builds the reconcile's UPDATE, minus the scope predicate its
// caller adds. Split out from closeOrphans so a test can EXPLAIN the real
// statement instead of a hand-written lookalike.
//
// It only writes disconnected_at. Sealing the query chain head is the caller's
// second and third statements, because a keyed stamp is HMAC(chain key, …) and
// the chain key lives only in this process — there is no SQL expression that
// can produce it. Until 0.24 the stamp *was* computed here, as a correlated
// subquery copying `mac` verbatim out of `queries`; that copy is exactly the
// forgery specs/todos/2026-08-10-06 closed.
func (s *Store) orphanCloseQuery(db bun.IDB) *bun.UpdateQuery {
	return db.NewUpdate().
		Model((*Connection)(nil)).
		Where("disconnected_at IS NULL").
		// last_activity_at, not now(): retention should measure from when the
		// session actually stopped talking, and a crashed session must not get
		// its clock reset by every subsequent restart.
		Set("disconnected_at = last_activity_at")
}

// orphanChainHead is one connection's recoverable chain head, as read back by
// orphanHeadSelect for either stamp writer. ChainSeq and MAC are NULL for a
// session that has logged nothing — the LEFT JOIN keeps the row rather than
// dropping it, so the caller can tell "nothing to seal" from "not asked about".
type orphanChainHead struct {
	ConnectionUID uuid.UUID `bun:"connection_uid"`
	ChainSeq      *int64    `bun:"chain_seq"`
	MAC           []byte    `bun:"mac"`
}

// stampOrphanHeads seals the query chain head of every connection the reconcile
// just closed, in the same transaction as the close.
//
// The head is recoverable without the process that died — it is the highest
// chain_seq on the connection and its MAC, which is exactly queryChainHeadSelect
// — so the reconcile does not need the in-memory chain state that died with it.
//
// Two caveats, both deliberate:
//
//   - The stamp seals whatever survived *at reconcile time*, not what the
//     session actually wrote. Someone who deleted trailing statements between
//     the crash and the reconcile gets the truncated chain blessed. That is
//     strictly better than no stamp at all — from the reconcile onward the tail
//     is protected — and it is why this shares the close's transaction: the
//     window is the reconcile itself, not the gap between two statements. A
//     close that committed without its stamp would leave rows that look
//     cleanly terminated and are sealed by nothing, and no later pass could
//     tell them apart from a session that logged nothing.
//   - A session that logged nothing is left unstamped (NULL mac, length 0,
//     version 0). There is no head to seal, and checkStampedHead skips a NULL
//     stamp rather than reading it as a break.
//
// No guard predicate: the rows were closed by the statement immediately before
// this one, in this transaction, so their state is already known.
func (s *Store) stampOrphanHeads(ctx context.Context, tx bun.Tx, uids []uuid.UUID) error {
	_, err := s.stampChainHeads(ctx, tx, uids, stampAnyState)

	return err
}

// Guard predicates stampChainHeads ANDs onto its write. They are constants, not
// caller-supplied SQL: the value is interpolated into the statement, so nothing
// dynamic may reach it.
const (
	// stampAnyState writes the stamp whatever state the row is in. The
	// reconcile's rows were closed one statement earlier in the same
	// transaction, so there is nothing left to race with.
	stampAnyState = ""

	// stampOnlyOpen writes the stamp only while the session is still open. It
	// is what keeps a lagging refresh from landing *after* a concurrent
	// CloseConnection and overwriting the exact final stamp with an older head
	// — which, once disconnected_at is set, checkStampedHead judges by the
	// exact rule and would report as a trailing deletion. Under READ COMMITTED
	// the losing UPDATE re-evaluates this predicate once it holds the row lock,
	// so the guard holds whichever of the two commits first.
	stampOnlyOpen = "c.disconnected_at IS NULL"
)

// chainStampBatchSize bounds how many connections one stamp statement carries.
// Each stamped row binds four parameters and PostgreSQL takes 65535 per
// statement, so the cap is really about a busy replica with thousands of open
// sessions rather than about the reconcile, whose input is whatever one pass
// closed. Batching also keeps a single sweep from locking every open connection
// row at once.
const chainStampBatchSize = 500

// stampChainHeads reads the query chain head of each given connection and writes
// the sealed stamp back, returning how many rows it stamped.
//
// It is the shared half of all three stamp writers that work off the database
// rather than off in-process chain state: the reconcile (stampOrphanHeads) and
// the live refresh (RefreshOpenChainStamps). CloseConnection is the third and
// stamps a single row from the head it already holds.
//
// A connection with no chained statement is skipped rather than stamped with a
// NULL head — there is nothing to seal, and checkMissingStamp reads a NULL
// stamp on a session with no surviving statements as "logged nothing" rather
// than as a break. That skip is what makes the converse safe: a *closed*
// session whose statements survive and whose stamp is NULL is a state no writer
// produces, so verification calls it tampering.
//
// Cost: one index lookup per *given* row — the LATERAL join runs its inner scan
// once per uid handed to it, not once per connection in the table. See
// TestQueryChainOrphanStampCostScalesWithOrphans and
// TestQueryChainRefreshCostScalesWithOpenSessions, which assert that against
// real plans.
func (s *Store) stampChainHeads(ctx context.Context, db bun.IDB, uids []uuid.UUID, guard string) (int64, error) {
	var stamped int64

	for start := 0; start < len(uids); start += chainStampBatchSize {
		end := min(start+chainStampBatchSize, len(uids))

		batch, err := s.stampChainHeadBatch(ctx, db, uids[start:end], guard)
		if err != nil {
			return stamped, err
		}

		stamped += batch
	}

	return stamped, nil
}

func (s *Store) stampChainHeadBatch(
	ctx context.Context, db bun.IDB, uids []uuid.UUID, guard string,
) (int64, error) {
	var heads []orphanChainHead

	if err := s.orphanHeadSelect(db, uids).Scan(ctx, &heads); err != nil {
		return 0, fmt.Errorf("failed to read the chain heads to stamp: %w", err)
	}

	// Four bound columns per stamped row: uid, mac, length, version.
	const stampColumns = 4

	values := make([]string, 0, len(heads))
	args := make([]any, 0, len(heads)*stampColumns)

	for _, head := range heads {
		if head.ChainSeq == nil || head.MAC == nil {
			continue
		}

		values = append(values, "(?::uuid, ?::bytea, ?::bigint, ?::smallint)")
		args = append(args,
			head.ConnectionUID,
			s.queryChainStampMAC(head.ConnectionUID, queryChainStampKeyed, *head.ChainSeq, head.MAC),
			*head.ChainSeq,
			queryChainStampKeyed)
	}

	if len(values) == 0 {
		return 0, nil
	}

	// v.len >= c.query_chain_len: the stamp never moves backwards.
	//
	// Chain positions only ever grow, and retention removes the *oldest*
	// statements, so it cannot lower a connection's highest chain_seq without
	// removing every one of them (in which case there is no head here to stamp
	// with and the row is skipped above). A recovered head that is lower than a
	// stamp already on the row therefore means statements were deleted from the
	// end — and overwriting the older, higher stamp with it would bless exactly
	// that. This is not hypothetical: a live session carries a refresh stamp
	// from the last sweep, and if its process then crashes the reconcile is the
	// next writer. Leaving the higher stamp is what makes verification report
	// the break instead.
	stamp := `UPDATE connections AS c
		   SET query_chain_mac = v.mac,
		       query_chain_len = v.len,
		       query_chain_stamp_version = v.version
		  FROM (VALUES ` + strings.Join(values, ", ") + `) AS v (uid, mac, len, version)
		 WHERE c.uid = v.uid AND v.len >= c.query_chain_len`

	if guard != stampAnyState {
		stamp += " AND " + guard
	}

	result, err := db.ExecContext(ctx, stamp, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to stamp the chain heads: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to count the stamped chain heads: %w", err)
	}

	return affected, nil
}

// RefreshOpenChainStamps re-seals the query chain head of every session this run
// still has open, and returns how many rows it stamped.
//
// Without it the stamp is only ever written by a *close* — CloseConnection or
// the reconcile — so a session that never ends has no stamp at all, and the
// stamp is the only thing that detects statements deleted from the *end* of a
// chain. A psql window left open all day, a pooled application connection or an
// approval hold waiting on a human is therefore unprotected at its tail for its
// whole life, and a crashed session is unprotected from the crash until the
// reclaim notices it (up to InstanceStaleAfter plus InstanceReclaimInterval).
// Running this on the reclaim timer bounds both windows by the sweep interval
// instead.
//
// Scoped to rows this run owns (run_id = s.runID). Two replicas sharing a store
// would otherwise both read and rewrite the same stamps every pass, for no gain:
// a row's head is best known to the process serving it, and a run id is minted
// in memory and cannot be shared. Rows predating run tracking carry NULL and are
// nobody's to refresh — they belong to a build that never wrote one.
//
// What the stamp then attests to is a *prefix*: the chain up to the position the
// last sweep sealed. Statements appended since are not covered, which is why
// checkStampedHead judges an open session by the prefix rule and only a closed
// one exactly. Deleting statements newer than the last sweep stays undetectable;
// this shrinks that window to the sweep interval rather than closing it.
func (s *Store) RefreshOpenChainStamps(ctx context.Context) (int64, error) {
	if !s.ChainEnabled() || s.runID == "" {
		return 0, nil
	}

	var uids []uuid.UUID

	if err := s.openChainStampSelect(s.db).Scan(ctx, &uids); err != nil {
		return 0, fmt.Errorf("failed to list the open connections to stamp: %w", err)
	}

	if len(uids) == 0 {
		return 0, nil
	}

	return s.stampChainHeads(ctx, s.db, uids, stampOnlyOpen)
}

// openChainStampSelect picks the connections one refresh sweep covers: still
// open, and owned by this run. Split out so a test can EXPLAIN the real
// statement — it must never touch `queries`, which is what makes the sweep cost
// one head lookup per open session rather than a scan over the store's history.
func (s *Store) openChainStampSelect(db bun.IDB) *bun.SelectQuery {
	return db.NewSelect().
		Model((*Connection)(nil)).
		Column("uid").
		Where("disconnected_at IS NULL").
		Where("run_id = ?", s.runID).
		Order("uid ASC")
}

// orphanHeadSelect reads the chain head of each of the given connections in one
// round trip: a VALUES list of the uids, LEFT JOIN LATERAL the same head lookup
// CloseConnection uses. Keeping it built from queryChainHeadSelect is what stops
// the reconcile's and the refresh sweep's notion of "the head" from drifting
// from the clean-close path's; the LATERAL shape is what keeps it one index
// lookup per uid instead of a scan over every chained statement in the store.
func (s *Store) orphanHeadSelect(db bun.IDB, uids []uuid.UUID) *bun.SelectQuery {
	placeholders := make([]string, len(uids))
	args := make([]any, len(uids))

	for i, uid := range uids {
		placeholders[i] = "(?::uuid)"
		args[i] = uid
	}

	head := queryChainHeadSelect(db, "chain_seq", "mac").Where("q.connection_id = v.uid")

	return db.NewSelect().
		ColumnExpr("v.uid AS connection_uid").
		ColumnExpr("h.chain_seq AS chain_seq").
		ColumnExpr("h.mac AS mac").
		TableExpr("(VALUES "+strings.Join(placeholders, ", ")+") AS v (uid)", args...).
		Join("LEFT JOIN LATERAL (?) AS h ON TRUE", head)
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
			"query_chain_mac, query_chain_len, query_chain_stamp_version").
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

// GetConnectionsByUIDs is the batched form of GetConnectionByUID: it reads
// every connection named in uids with exactly one query, however many uids are
// asked about, and returns them keyed by uid.
//
// Same projection as GetConnectionByUID, deliberately — a batched read must not
// see a different row shape than the per-row one, since callers switch between
// them (see the pending-approvals listing, which resolves the grant governing a
// whole page from this map and then runs the *same* decision function over it).
//
// A uid that names no row is simply absent from the map; the caller reads that
// as the fail-closed equivalent of GetConnectionByUID's ErrConnectionNotFound.
func (s *Store) GetConnectionsByUIDs(ctx context.Context, uids []uuid.UUID) (map[uuid.UUID]*Connection, error) {
	result := make(map[uuid.UUID]*Connection, len(uids))

	if len(uids) == 0 {
		return result, nil
	}

	var connections []Connection

	err := s.db.NewSelect().
		Model(&connections).
		ColumnExpr("uid, user_id, database_id, source_ip::text, connected_at, last_activity_at, "+
			"disconnected_at, queries, bytes_transferred, instance_id, upstream_tls, dump_key, grant_uid, "+
			"query_chain_mac, query_chain_len, query_chain_stamp_version").
		Where("uid IN (?)", bun.List(uids)).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get connections by uids: %w", err)
	}

	for i := range connections {
		result[connections[i].UID] = &connections[i]
	}

	return result, nil
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
