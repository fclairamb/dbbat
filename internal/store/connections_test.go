package store

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateConnection(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "conn")

	t.Run("create connection", func(t *testing.T) {
		conn, err := store.CreateConnection(ctx, user.UID, database.UID, "192.168.1.100")
		if err != nil {
			t.Fatalf("CreateConnection() error = %v", err)
		}

		if conn.UID == uuid.Nil {
			t.Error("CreateConnection() conn.UID = uuid.Nil")
		}
		if conn.UserID != user.UID {
			t.Errorf("CreateConnection() conn.UserID = %s, want %s", conn.UserID, user.UID)
		}
		if conn.DatabaseID != database.UID {
			t.Errorf("CreateConnection() conn.DatabaseID = %s, want %s", conn.DatabaseID, database.UID)
		}
		// PostgreSQL INET type may include CIDR suffix
		if !strings.HasPrefix(conn.SourceIP, "192.168.1.100") {
			t.Errorf("CreateConnection() conn.SourceIP = %q, want prefix %q", conn.SourceIP, "192.168.1.100")
		}
		if conn.ConnectedAt.IsZero() {
			t.Error("CreateConnection() conn.ConnectedAt is zero")
		}
		if conn.DisconnectedAt != nil {
			t.Error("CreateConnection() conn.DisconnectedAt should be nil")
		}
	})

	// upstream_tls is what makes the opportunistic ssl_mode fallback auditable
	// instead of silent, so it has to survive the round trip rather than be a
	// field the insert quietly drops.
	t.Run("records the upstream encryption state", func(t *testing.T) {
		for _, encrypted := range []bool{true, false} {
			conn, err := store.CreateConnection(ctx, user.UID, database.UID, "192.168.1.101",
				WithUpstreamTLS(encrypted))
			if err != nil {
				t.Fatalf("CreateConnection() error = %v", err)
			}

			if conn.UpstreamTLS != encrypted {
				t.Errorf("returned conn.UpstreamTLS = %v, want %v", conn.UpstreamTLS, encrypted)
			}

			reread, err := store.GetConnectionByUID(ctx, conn.UID)
			if err != nil {
				t.Fatalf("GetConnectionByUID() error = %v", err)
			}

			if reread.UpstreamTLS != encrypted {
				t.Errorf("persisted conn.UpstreamTLS = %v, want %v", reread.UpstreamTLS, encrypted)
			}
		}
	})

	t.Run("defaults the upstream encryption state to false", func(t *testing.T) {
		conn, err := store.CreateConnection(ctx, user.UID, database.UID, "192.168.1.102")
		if err != nil {
			t.Fatalf("CreateConnection() error = %v", err)
		}

		if conn.UpstreamTLS {
			t.Error("a connection with no recorded encryption state must not claim to be encrypted")
		}
	})

	t.Run("create connection with IPv6", func(t *testing.T) {
		conn, err := store.CreateConnection(ctx, user.UID, database.UID, "::1")
		if err != nil {
			t.Fatalf("CreateConnection() error = %v", err)
		}

		// PostgreSQL INET type may include CIDR suffix
		if !strings.HasPrefix(conn.SourceIP, "::1") {
			t.Errorf("CreateConnection() conn.SourceIP = %q, want prefix %q", conn.SourceIP, "::1")
		}
	})
}

func TestCloseConnection(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "close")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	t.Run("close open connection", func(t *testing.T) {
		err := store.CloseConnection(ctx, conn.UID)
		if err != nil {
			t.Fatalf("CloseConnection() error = %v", err)
		}

		// Verify connection is closed by listing
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}

		found := false
		for _, c := range conns {
			if c.UID == conn.UID {
				found = true
				if c.DisconnectedAt == nil {
					t.Error("conn.DisconnectedAt should not be nil after close")
				}
				break
			}
		}
		if !found {
			t.Error("connection not found after close")
		}
	})

	t.Run("close already closed connection", func(t *testing.T) {
		err := store.CloseConnection(ctx, conn.UID)
		if !errors.Is(err, ErrConnectionNotFound) {
			t.Errorf("CloseConnection() error = %v, want %v", err, ErrConnectionNotFound)
		}
	})

	t.Run("close non-existing connection", func(t *testing.T) {
		err := store.CloseConnection(ctx, uuid.New())
		if !errors.Is(err, ErrConnectionNotFound) {
			t.Errorf("CloseConnection() error = %v, want %v", err, ErrConnectionNotFound)
		}
	})
}

// backdateConnection rewrites a connection's clock so a session that stopped
// talking in the past can be simulated: CreateConnection always stamps "now".
func backdateConnection(t *testing.T, ctx context.Context, store *Store, uid uuid.UUID, at time.Time) {
	t.Helper()

	_, err := store.db.ExecContext(ctx,
		"UPDATE connections SET connected_at = ?, last_activity_at = ? WHERE uid = ?", at, at, uid)
	require.NoError(t, err)
}

// registerInstanceAs writes a registry row for another run — another instance
// id, or another run of ours — last seen lastSeenAgo before now, without
// disturbing the store's own identity. Zero means "alive and heartbeating right
// now"; more than InstanceStaleAfter means "gone".
//
// The backdating is done in SQL, against the same database clock the heartbeat
// and the staleness cutoff use — a test that mixed in the Go clock would be
// testing a time base the code no longer has.
func registerInstanceAs(
	t *testing.T, ctx context.Context, store *Store, instanceID, runID string, lastSeenAgo time.Duration,
) {
	t.Helper()

	asRun(t, store, instanceID, runID, func() {
		require.NoError(t, store.RegisterInstance(ctx))
	})

	_, err := store.db.ExecContext(ctx,
		"UPDATE instances SET last_seen_at = now() - make_interval(secs => ?) WHERE instance_id = ? AND run_id = ?",
		lastSeenAgo.Seconds(), instanceID, runID)
	require.NoError(t, err)
}

// asRun runs fn with the store impersonating one (instance id, run id) pair,
// then puts its own identity back. An empty run id means "a build that predates
// run tracking": the rows it writes carry a NULL run_id.
func asRun(t *testing.T, store *Store, instanceID, runID string, fn func()) {
	t.Helper()

	previousInstance, previousRun := store.InstanceID(), store.RunID()

	store.SetInstanceID(instanceID)
	store.SetRunID(runID)

	defer func() {
		store.SetInstanceID(previousInstance)
		store.SetRunID(previousRun)
	}()

	fn()
}

// TestCloseOrphanedConnections covers the startup reconcile of connections a
// previous run left open — including the guarantee that it can never touch a
// connection belonging to another replica sharing the same store.
func TestCloseOrphanedConnections(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "orphans")

	// Postgres timestamptz keeps microseconds; truncate so comparisons are
	// about the value rather than Go's nanosecond tail.
	stopped := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)
	cleanlyClosedAt := time.Now().Add(-36 * time.Hour).Truncate(time.Microsecond)

	var orphan, alreadyClosed, otherReplica *Connection

	// (1) The crash orphan: opened by a previous run of this instance id, never
	// closed. That run left no registry row behind, so it is provably over.
	asRun(t, store, "instance-a", "run-1", func() {
		var err error

		orphan, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
		require.NoError(t, err)

		// (2) A connection that run already closed cleanly.
		alreadyClosed, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
		require.NoError(t, err)
	})

	backdateConnection(t, ctx, store, orphan.UID, stopped)
	backdateConnection(t, ctx, store, alreadyClosed.UID, stopped)
	_, err := store.db.ExecContext(ctx,
		"UPDATE connections SET disconnected_at = ? WHERE uid = ?", cleanlyClosedAt, alreadyClosed.UID)
	require.NoError(t, err)

	// (3) A live connection owned by another replica sharing this store. It is
	// registered and heartbeating, which is what marks it as alive.
	asRun(t, store, "instance-b", "run-b", func() {
		var err error

		otherReplica, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.3")
		require.NoError(t, err)
	})

	backdateConnection(t, ctx, store, otherReplica.UID, stopped)

	// This run: the same instance id as (1), a new run id — a restart.
	store.SetInstanceID("instance-a")
	store.SetRunID("run-2")
	registerInstanceAs(t, ctx, store, "instance-b", "run-b", 0)
	require.NoError(t, store.RegisterInstance(ctx))

	closed, err := store.CloseOrphanedConnections(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), closed.Own,
		"only the still-open connection of instance-a's previous run should be reconciled")
	assert.Equal(t, int64(0), closed.Reclaimed, "a heartbeating instance's connections are never reclaimed")

	t.Run("an open connection is closed at its last_activity_at", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, orphan.UID)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt, "the orphan should no longer look open")
		// last_activity_at, not now(): retention has to measure from when the
		// session actually stopped talking.
		assert.WithinDuration(t, stopped, *got.DisconnectedAt, time.Millisecond)
		assert.False(t, got.DisconnectedAt.After(time.Now().Add(-24*time.Hour)),
			"disconnected_at must not be reset to the restart time")
	})

	t.Run("an already-closed connection keeps its original timestamp", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, alreadyClosed.UID)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt)
		assert.WithinDuration(t, cleanlyClosedAt, *got.DisconnectedAt, time.Millisecond,
			"the reconcile must not overwrite a clean teardown's timestamp")
	})

	t.Run("another instance's live connection is untouched", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, otherReplica.UID)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt,
			"starting one replica must never close another replica's live connections")
		assert.Equal(t, "instance-b", got.InstanceID)
	})

	t.Run("running it again is a no-op", func(t *testing.T) {
		again, err := store.CloseOrphanedConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), again.Total())
	})

	t.Run("an unset instance id reconciles nothing", func(t *testing.T) {
		// Refusing here is what keeps an unconfigured process from performing
		// the blanket update the instance scoping exists to prevent.
		store.SetInstanceID("")

		none, err := store.CloseOrphanedConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), none.Total())

		got, err := store.GetConnectionByUID(ctx, otherReplica.UID)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt)
	})
}

// TestCloseOrphanedConnectionsReclaimsDeadInstances covers the liveness half of
// the reconcile: connections whose owning instance is provably gone are closed
// too, while a heartbeating instance's are not.
func TestCloseOrphanedConnectionsReclaimsDeadInstances(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "reclaim")

	stopped := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)

	// openConnectionFor opens a connection stamped with someone else's identity,
	// backdated so it looks like a session that stopped talking long ago. An
	// empty run id writes a NULL one: a row from before run tracking existed.
	openConnectionFor := func(instanceID, runID, sourceIP string) uuid.UUID {
		t.Helper()

		var conn *Connection

		asRun(t, store, instanceID, runID, func() {
			var err error

			conn, err = store.CreateConnection(ctx, user.UID, database.UID, sourceIP)
			require.NoError(t, err)
		})

		backdateConnection(t, ctx, store, conn.UID, stopped)

		return conn.UID
	}

	// A replica that is up and heartbeating. Its sessions must survive: closing
	// them would let the retention sweep delete a connection a live session is
	// still writing queries against.
	registerInstanceAs(t, ctx, store, "live-instance", "live-run", 0)
	liveConn := openConnectionFor("live-instance", "live-run", "10.1.0.1")

	// A replica that registered and then stopped heartbeating — a crashed pod.
	registerInstanceAs(t, ctx, store, "stale-instance", "stale-run", InstanceStaleAfter+time.Minute)
	staleConn := openConnectionFor("stale-instance", "stale-run", "10.1.0.2")

	// A replica that shut down cleanly and deleted its registration. No row at
	// all, so it is gone immediately rather than after the grace period.
	registerInstanceAs(t, ctx, store, "gone-instance", "gone-run", 0)
	goneConn := openConnectionFor("gone-instance", "gone-run", "10.1.0.3")
	asRun(t, store, "gone-instance", "gone-run", func() {
		require.NoError(t, store.DeregisterInstance(ctx))
	})

	// A legacy row from before the instance_id column existed. Deliberately
	// folded into the "no registry row" case: '' is never a live owner, since
	// no process can register it.
	legacyConn := openConnectionFor("", "", "10.1.0.4")

	// And a leftover of a previous run of this instance id, which is counted
	// separately.
	store.SetInstanceID("reclaimer")
	store.SetRunID("reclaimer-run")
	require.NoError(t, store.RegisterInstance(ctx))

	ownConn := openConnectionFor("reclaimer", "reclaimer-previous-run", "10.1.0.5")

	closed, err := store.CloseOrphanedConnections(ctx)
	require.NoError(t, err)

	assert.Equal(t, int64(1), closed.Own, "own leftovers are counted on their own")
	assert.Equal(t, int64(3), closed.Reclaimed,
		"the stale, the deregistered and the legacy connection are reclaimed — not the live one")

	t.Run("a live instance's connections are untouched", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, liveConn)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt,
			"a recently heartbeating instance's session must never be closed by another instance")
	})

	closedAt := func(uid uuid.UUID) time.Time {
		t.Helper()

		got, err := store.GetConnectionByUID(ctx, uid)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt, "the connection should no longer look open")

		return *got.DisconnectedAt
	}

	t.Run("a stale instance's connections are reclaimed", func(t *testing.T) {
		assert.WithinDuration(t, stopped, closedAt(staleConn), time.Millisecond)
	})

	t.Run("a deregistered instance's connections are reclaimed", func(t *testing.T) {
		assert.WithinDuration(t, stopped, closedAt(goneConn), time.Millisecond)
	})

	t.Run("legacy rows with an empty instance id are reclaimed", func(t *testing.T) {
		assert.WithinDuration(t, stopped, closedAt(legacyConn), time.Millisecond)
	})

	t.Run("reclaimed connections keep last_activity_at as the timestamp", func(t *testing.T) {
		// Not now(): retention has to measure from when the session actually
		// stopped talking, whoever it belonged to.
		for _, uid := range []uuid.UUID{staleConn, goneConn, legacyConn, ownConn} {
			at := closedAt(uid)
			assert.WithinDuration(t, stopped, at, time.Millisecond)
			assert.True(t, at.Before(time.Now().Add(-24*time.Hour)),
				"disconnected_at must not be reset to the reclaim time")
		}
	})

	t.Run("running it again reclaims nothing", func(t *testing.T) {
		again, err := store.CloseOrphanedConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), again.Total())
	})
}

// TestReclaimDeadInstanceConnectionsWithoutStartup covers the periodic reclaim:
// the same liveness-checked sweep, run from a long-running process rather than
// from the startup reconcile.
//
// That is the case the startup pass structurally cannot serve. A SIGKILLed pod
// leaves a registry row seconds old, so its replacement — starting immediately
// — reclaims nothing; only a later pass, once the row has gone stale, does. So
// this test never calls CloseOrphanedConnections: the reclaim has to stand on
// its own, without the own-instance update, which mid-life would close this
// run's own live sessions.
func TestReclaimDeadInstanceConnectionsWithoutStartup(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "periodic-reclaim")

	stopped := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)

	openConnectionFor := func(instanceID, runID, sourceIP string) uuid.UUID {
		t.Helper()

		var conn *Connection

		asRun(t, store, instanceID, runID, func() {
			var err error

			conn, err = store.CreateConnection(ctx, user.UID, database.UID, sourceIP)
			require.NoError(t, err)
		})

		backdateConnection(t, ctx, store, conn.UID, stopped)

		return conn.UID
	}

	// This process: registered, heartbeating, and serving a session of its own.
	// The periodic reclaim runs while that session is live, so leaving it alone
	// is the property that makes running this outside startup safe at all.
	store.SetInstanceID("running-instance")
	store.SetRunID("current-run")
	require.NoError(t, store.RegisterInstance(ctx))

	ownLive, err := store.CreateConnection(ctx, user.UID, database.UID, "10.2.0.1")
	require.NoError(t, err)

	// A peer that is up and heartbeating.
	registerInstanceAs(t, ctx, store, "live-peer", "live-peer-run", 0)
	peerConn := openConnectionFor("live-peer", "live-peer-run", "10.2.0.2")

	// A peer that crashed: its row is still there, but long past the grace
	// period. This is what only the periodic pass catches.
	registerInstanceAs(t, ctx, store, "crashed-peer", "crashed-peer-run", InstanceStaleAfter+time.Minute)
	crashedConn := openConnectionFor("crashed-peer", "crashed-peer-run", "10.2.0.3")

	// A previous run of *our own* instance id that crashed long enough ago to be
	// past the grace period. The startup pass could not have taken it — when we
	// started, its row was still fresh — and no other process will, because the
	// id is ours. So this pass has to, which is why its scope excludes this run
	// rather than this instance id.
	registerInstanceAs(t, ctx, store, "running-instance", "previous-run", InstanceStaleAfter+time.Minute)
	previousRunConn := openConnectionFor("running-instance", "previous-run", "10.2.0.5")

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), reclaimed,
		"the crashed peer's and our own dead previous run's connections should be reclaimed")

	t.Run("the crashed instance's connection is closed at its last activity", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, crashedConn)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt)
		assert.WithinDuration(t, stopped, *got.DisconnectedAt, time.Millisecond)
	})

	t.Run("a live instance's connection is untouched", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, peerConn)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt,
			"a heartbeating instance's session must survive every periodic pass")
	})

	t.Run("a dead previous run of our own instance id is reclaimed", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, previousRunConn)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt,
			"a stable instance id must not keep its own crashed run's rows open until the next restart")
		assert.WithinDuration(t, stopped, *got.DisconnectedAt, time.Millisecond)
	})

	t.Run("this run's own live connection is untouched", func(t *testing.T) {
		// The line the reclaim draws is this run, not this instance id: our own
		// open rows are sessions we are serving right now.
		got, err := store.GetConnectionByUID(ctx, ownLive.UID)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt,
			"the periodic reclaim must never close the calling process's own sessions")
	})

	t.Run("running it again reclaims nothing", func(t *testing.T) {
		// Every replica runs this on its own timer, so overlapping passes are
		// expected; the second one has to be a no-op rather than double work.
		again, err := store.ReclaimDeadInstanceConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), again)
	})

	t.Run("an unset instance id reclaims nothing", func(t *testing.T) {
		store.SetInstanceID("")

		registerInstanceAs(t, ctx, store, "another-crashed-peer", "another-crashed-run",
			InstanceStaleAfter+time.Minute)
		orphan := openConnectionFor("another-crashed-peer", "another-crashed-run", "10.2.0.4")

		none, err := store.ReclaimDeadInstanceConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), none, "a process with no identity judges nobody")

		got, err := store.GetConnectionByUID(ctx, orphan)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt)
	})
}

// openBackdatedConnection opens a connection under a given (instance id, run
// id) and backdates it, so it looks like a session that stopped talking at
// `stopped`. An empty run id writes a NULL one: a row from a build that
// predates run tracking.
func openBackdatedConnection(
	t *testing.T, ctx context.Context, store *Store,
	user, database uuid.UUID, instanceID, runID, sourceIP string, stopped time.Time,
) uuid.UUID {
	t.Helper()

	var conn *Connection

	asRun(t, store, instanceID, runID, func() {
		var err error

		conn, err = store.CreateConnection(ctx, user, database, sourceIP)
		require.NoError(t, err)
	})

	backdateConnection(t, ctx, store, conn.UID, stopped)

	return conn.UID
}

// TestCloseOrphanedConnectionsWithSharedInstanceID is the case run ids exist
// for: two live processes carrying one DBB_INSTANCE_ID, one of them starting.
//
// Before run ids the starting one closed every open row stamped with the id,
// its peer's live sessions included — and a closed row is immediately eligible
// for the retention sweep, which then deletes a connection a live session is
// still writing queries against. Identity is not enough to prevent that,
// because an instance id is unique per process only by convention.
func TestCloseOrphanedConnectionsWithSharedInstanceID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "shared-id")

	stopped := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)

	// The peer: live, heartbeating, serving a session — under the very instance
	// id we are about to start with.
	registerInstanceAs(t, ctx, store, "shared", "peer-run", 0)
	peerConn := openBackdatedConnection(t, ctx, store, user.UID, database.UID,
		"shared", "peer-run", "10.3.0.1", stopped)

	// Our own predecessor, which crashed moments ago: same id, another run, and
	// a registry row that is still well inside the grace period.
	registerInstanceAs(t, ctx, store, "shared", "previous-run", 0)
	previousConn := openBackdatedConnection(t, ctx, store, user.UID, database.UID,
		"shared", "previous-run", "10.3.0.2", stopped)

	store.SetInstanceID("shared")
	store.SetRunID("our-run")
	require.NoError(t, store.RegisterInstance(ctx))

	closed, err := store.CloseOrphanedConnections(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), closed.Total(),
		"nothing under a shared id may be closed while every run of it still looks alive")

	t.Run("a live peer sharing our instance id keeps its session", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, peerConn)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt,
			"starting a replica must never close a live peer's session, shared id or not")
	})

	t.Run("our own predecessor is spared while its registry row is fresh", func(t *testing.T) {
		// The price of not trusting the id: at startup a crashed predecessor
		// and a live peer look identical, so the predecessor waits out the
		// grace period like any other dead run instead of being closed now.
		got, err := store.GetConnectionByUID(ctx, previousConn)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt)
	})

	t.Run("registering does not overwrite a peer's heartbeat", func(t *testing.T) {
		// Keyed by the pair, so all three runs of this id are represented. With
		// one row per id, our registration would have replaced the peer's — and
		// a peer with no row of its own has every session it is serving
		// reclaimed by the next pass.
		var runs int

		require.NoError(t, store.db.NewSelect().
			Model((*Instance)(nil)).
			ColumnExpr("count(*)").
			Where("instance_id = ?", "shared").
			Scan(ctx, &runs))
		assert.Equal(t, 3, runs)
	})

	t.Run("the predecessor is reclaimed once it goes stale", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx,
			"UPDATE instances SET last_seen_at = now() - make_interval(secs => ?) WHERE run_id = ?",
			(InstanceStaleAfter + time.Minute).Seconds(), "previous-run")
		require.NoError(t, err)

		reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), reclaimed,
			"a dead run of our own id is reclaimed by the periodic pass, not left open forever")

		gone, err := store.GetConnectionByUID(ctx, previousConn)
		require.NoError(t, err)
		require.NotNil(t, gone.DisconnectedAt)
		assert.WithinDuration(t, stopped, *gone.DisconnectedAt, time.Millisecond)

		alive, err := store.GetConnectionByUID(ctx, peerConn)
		require.NoError(t, err)
		assert.Nil(t, alive.DisconnectedAt, "the live peer is still off limits")
	})
}

// TestCloseOrphanedConnectionsWithoutRunIDs covers the rows written before the
// run_id column existed, which carry NULL.
//
// They are judged by the rule their owner is playing by — a build that only
// maintains the per-id registry — so any live run of their instance id keeps
// them. That is the pre-existing behavior, kept deliberately: the stricter
// per-run test would have declared every one of them ownerless the moment the
// migration landed, including the sessions an old replica was still serving
// through the upgrade.
//
// The one exception is our own registration, which never vouches for a row it
// did not open. Without it, restarting under a stable instance id would shield
// its own pre-upgrade orphans forever — the leak all of this exists to close.
func TestCloseOrphanedConnectionsWithoutRunIDs(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "null-run")

	stopped := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)

	ownLegacy := openBackdatedConnection(t, ctx, store, user.UID, database.UID,
		"legacy-self", "", "10.4.0.1", stopped)

	registerInstanceAs(t, ctx, store, "legacy-peer", "legacy-peer-run", 0)
	peerLegacy := openBackdatedConnection(t, ctx, store, user.UID, database.UID,
		"legacy-peer", "", "10.4.0.2", stopped)

	registerInstanceAs(t, ctx, store, "legacy-dead", "legacy-dead-run", InstanceStaleAfter+time.Minute)
	deadLegacy := openBackdatedConnection(t, ctx, store, user.UID, database.UID,
		"legacy-dead", "", "10.4.0.3", stopped)

	store.SetInstanceID("legacy-self")
	store.SetRunID("our-run")
	require.NoError(t, store.RegisterInstance(ctx))

	closed, err := store.CloseOrphanedConnections(ctx)
	require.NoError(t, err)

	t.Run("our own pre-upgrade rows are closed", func(t *testing.T) {
		assert.Equal(t, int64(1), closed.Own,
			"our fresh registration must not vouch for rows a previous build left behind")

		got, err := store.GetConnectionByUID(ctx, ownLegacy)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt)
		assert.WithinDuration(t, stopped, *got.DisconnectedAt, time.Millisecond)
	})

	t.Run("a live instance's pre-upgrade rows are kept", func(t *testing.T) {
		assert.Equal(t, int64(1), closed.Reclaimed, "only the dead instance's row is reclaimed")

		got, err := store.GetConnectionByUID(ctx, peerLegacy)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt,
			"a replica mid-upgrade heartbeats its id but stamps no run id; its sessions are live")
	})

	t.Run("a dead instance's pre-upgrade rows are reclaimed", func(t *testing.T) {
		got, err := store.GetConnectionByUID(ctx, deadLegacy)
		require.NoError(t, err)
		require.NotNil(t, got.DisconnectedAt)
		assert.WithinDuration(t, stopped, *got.DisconnectedAt, time.Millisecond)
	})
}

// TestCloseOrphanedConnectionsAreReapedByRetention is the point of the whole
// exercise: an orphan is invisible to the retention sweep until the reconcile
// gives it a disconnected_at, and reapable immediately afterwards.
func TestCloseOrphanedConnectionsAreReapedByRetention(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "orphan-reap")

	store.SetInstanceID("instance-a")
	store.SetRunID("run-2")

	// Opened by the run this one replaced, which left no registry row behind.
	var orphan *Connection

	asRun(t, store, "instance-a", "run-1", func() {
		var err error

		orphan, err = store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.9")
		require.NoError(t, err)
	})

	long := time.Now().Add(-48 * time.Hour)
	createQueryWithRows(t, ctx, store, orphan.UID, "SELECT 'orphan'", long)
	backdateConnection(t, ctx, store, orphan.UID, long)

	// Before the reconcile the row still looks live, so the sweep leaves it
	// alone however old it is — that is exactly the leak.
	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.Connections, "an open connection is never reaped")

	_, err = store.GetConnectionByUID(ctx, orphan.UID)
	require.NoError(t, err, "the orphan survives the sweep while disconnected_at is NULL")

	closed, err := store.CloseOrphanedConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), closed.Own)

	// Now it is past the cutoff and closed, so the next sweep takes it.
	result, err = store.CleanupOldQueryRows(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Connections)

	_, err = store.GetConnectionByUID(ctx, orphan.UID)
	require.ErrorIs(t, err, ErrConnectionNotFound, "the reconciled orphan should be gone")
}

func TestGetConnectionByUID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "getconn")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.5")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	t.Run("get existing connection", func(t *testing.T) {
		found, err := store.GetConnectionByUID(ctx, conn.UID)
		if err != nil {
			t.Fatalf("GetConnectionByUID() error = %v", err)
		}
		if found.UID != conn.UID {
			t.Errorf("GetConnectionByUID() UID = %s, want %s", found.UID, conn.UID)
		}
		if found.UserID != user.UID {
			t.Errorf("GetConnectionByUID() UserID = %s, want %s", found.UserID, user.UID)
		}
		if !strings.HasPrefix(found.SourceIP, "10.0.0.5") {
			t.Errorf("GetConnectionByUID() SourceIP = %q, want prefix %q", found.SourceIP, "10.0.0.5")
		}
	})

	t.Run("get non-existing connection", func(t *testing.T) {
		_, err := store.GetConnectionByUID(ctx, uuid.New())
		if !errors.Is(err, ErrConnectionNotFound) {
			t.Errorf("GetConnectionByUID() error = %v, want %v", err, ErrConnectionNotFound)
		}
	})
}

func TestListConnections(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user1, db1 := createTestUserAndDatabase(t, ctx, store, "listc1")
	user2, db2 := createTestUserAndDatabase(t, ctx, store, "listc2")

	// Create connections
	_, err := store.CreateConnection(ctx, user1.UID, db1.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	_, err = store.CreateConnection(ctx, user1.UID, db2.UID, "10.0.0.2")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	_, err = store.CreateConnection(ctx, user2.UID, db1.UID, "10.0.0.3")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	t.Run("list all", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 3 {
			t.Errorf("ListConnections() len = %d, want 3", len(conns))
		}
	})

	t.Run("filter by user", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 2 {
			t.Errorf("ListConnections() len = %d, want 2", len(conns))
		}
	})

	t.Run("filter by database", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 2 {
			t.Errorf("ListConnections() len = %d, want 2", len(conns))
		}
	})

	t.Run("with limit", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{Limit: 2})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 2 {
			t.Errorf("ListConnections() len = %d, want 2", len(conns))
		}
	})

	t.Run("with offset", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{Limit: 10, Offset: 2})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 1 {
			t.Errorf("ListConnections() len = %d, want 1", len(conns))
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user1.UID, DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 1 {
			t.Errorf("ListConnections() len = %d, want 1", len(conns))
		}
	})
}

func TestIncrementConnectionStats(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "stats")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	// Verify initial values are zero
	if conn.Queries != 0 {
		t.Errorf("Initial conn.Queries = %d, want 0", conn.Queries)
	}
	if conn.BytesTransferred != 0 {
		t.Errorf("Initial conn.BytesTransferred = %d, want 0", conn.BytesTransferred)
	}

	t.Run("increment stats", func(t *testing.T) {
		err := store.IncrementConnectionStats(ctx, conn.UID, 1024)
		if err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}

		// Fetch and verify
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}

		var found *Connection
		for i := range conns {
			if conns[i].UID == conn.UID {
				found = &conns[i]
				break
			}
		}
		if found == nil {
			t.Fatal("connection not found")
		}

		if found.Queries != 1 {
			t.Errorf("conn.Queries = %d, want 1", found.Queries)
		}
		if found.BytesTransferred != 1024 {
			t.Errorf("conn.BytesTransferred = %d, want 1024", found.BytesTransferred)
		}
	})

	t.Run("multiple increments accumulate", func(t *testing.T) {
		err := store.IncrementConnectionStats(ctx, conn.UID, 2048)
		if err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
		err = store.IncrementConnectionStats(ctx, conn.UID, 512)
		if err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}

		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}

		var found *Connection
		for i := range conns {
			if conns[i].UID == conn.UID {
				found = &conns[i]
				break
			}
		}
		if found == nil {
			t.Fatal("connection not found")
		}

		// 1 + 2 = 3 queries total
		if found.Queries != 3 {
			t.Errorf("conn.Queries = %d, want 3", found.Queries)
		}
		// 1024 + 2048 + 512 = 3584 bytes total
		if found.BytesTransferred != 3584 {
			t.Errorf("conn.BytesTransferred = %d, want 3584", found.BytesTransferred)
		}
	})
}

func TestIncrementConnectionBytes(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "bytesonly")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	findConn := func() *Connection {
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		for i := range conns {
			if conns[i].UID == conn.UID {
				return &conns[i]
			}
		}
		t.Fatal("connection not found")
		return nil
	}

	t.Run("adds bytes without bumping query count", func(t *testing.T) {
		if err := store.IncrementConnectionBytes(ctx, conn.UID, 4096); err != nil {
			t.Fatalf("IncrementConnectionBytes() error = %v", err)
		}

		found := findConn()
		if found.BytesTransferred != 4096 {
			t.Errorf("conn.BytesTransferred = %d, want 4096", found.BytesTransferred)
		}
		if found.Queries != 0 {
			t.Errorf("conn.Queries = %d, want 0 (bytes-only increment must not bump query count)", found.Queries)
		}
	})

	t.Run("accumulates and coexists with IncrementConnectionStats", func(t *testing.T) {
		// A completed query bumps both counters...
		if err := store.IncrementConnectionStats(ctx, conn.UID, 1000); err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
		// ...then an aborted-query byte flush adds bytes only.
		if err := store.IncrementConnectionBytes(ctx, conn.UID, 500); err != nil {
			t.Fatalf("IncrementConnectionBytes() error = %v", err)
		}

		found := findConn()
		// 4096 + 1000 + 500
		if found.BytesTransferred != 5596 {
			t.Errorf("conn.BytesTransferred = %d, want 5596", found.BytesTransferred)
		}
		// Only the IncrementConnectionStats call bumps queries.
		if found.Queries != 1 {
			t.Errorf("conn.Queries = %d, want 1", found.Queries)
		}
	})
}

func TestExtractSourceIP(t *testing.T) {
	tests := []struct {
		name     string
		addr     net.Addr
		expected string
	}{
		{
			name:     "TCP IPv4",
			addr:     &net.TCPAddr{IP: net.ParseIP("192.168.1.1"), Port: 12345},
			expected: "192.168.1.1",
		},
		{
			name:     "TCP IPv6",
			addr:     &net.TCPAddr{IP: net.ParseIP("::1"), Port: 12345},
			expected: "::1",
		},
		{
			name:     "TCP IPv6 full",
			addr:     &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 8080},
			expected: "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractSourceIP(tt.addr)
			if result != tt.expected {
				t.Errorf("ExtractSourceIP() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestConnectionDumpKeyIsSelectedEverywhere pins the one thing an explicit
// ColumnExpr list gets wrong silently: a column left out of it scans as the
// zero value, so a caller reading DumpKey off a listed row would see "no
// capture uploaded" for a capture that is sitting in the bucket. Both read
// paths are asserted together, because forgetting one of the two is exactly
// the mistake this guards against.
func TestConnectionDumpKeyIsSelectedEverywhere(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "dumpkey")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.9")
	require.NoError(t, err)
	assert.Empty(t, conn.DumpKey, "a fresh connection has no uploaded capture")

	const key = "2026/08/05/instance-a/" + "capture.pcapng"

	require.NoError(t, store.SetConnectionDumpKey(ctx, conn.UID, key))

	found, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	assert.Equal(t, key, found.DumpKey)

	listed, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, key, listed[0].DumpKey)

	// Clearing it — what deleting a capture does — must round-trip too.
	require.NoError(t, store.ClearConnectionDumpKey(ctx, conn.UID))

	listed, err = store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Empty(t, listed[0].DumpKey)
}
