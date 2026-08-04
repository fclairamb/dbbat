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

// registerInstanceAs writes a registry row for another instance id, last seen
// lastSeenAgo before now, without disturbing the store's own instance id. Zero
// means "alive and heartbeating right now"; more than InstanceStaleAfter means
// "gone".
//
// The backdating is done in SQL, against the same database clock the heartbeat
// and the staleness cutoff use — a test that mixed in the Go clock would be
// testing a time base the code no longer has.
func registerInstanceAs(t *testing.T, ctx context.Context, store *Store, instanceID string, lastSeenAgo time.Duration) {
	t.Helper()

	previous := store.InstanceID()
	store.SetInstanceID(instanceID)
	require.NoError(t, store.RegisterInstance(ctx))
	store.SetInstanceID(previous)

	_, err := store.db.ExecContext(ctx,
		"UPDATE instances SET last_seen_at = now() - make_interval(secs => ?) WHERE instance_id = ?",
		lastSeenAgo.Seconds(), instanceID)
	require.NoError(t, err)
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

	// (1) The crash orphan: opened by this instance, never closed.
	store.SetInstanceID("instance-a")

	orphan, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	backdateConnection(t, ctx, store, orphan.UID, stopped)

	// (2) A connection this instance already closed cleanly.
	alreadyClosed, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
	require.NoError(t, err)
	backdateConnection(t, ctx, store, alreadyClosed.UID, stopped)
	_, err = store.db.ExecContext(ctx,
		"UPDATE connections SET disconnected_at = ? WHERE uid = ?", cleanlyClosedAt, alreadyClosed.UID)
	require.NoError(t, err)

	// (3) A live connection owned by another replica sharing this store. It is
	// registered and heartbeating, which is what marks it as alive.
	store.SetInstanceID("instance-b")

	otherReplica, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.3")
	require.NoError(t, err)
	backdateConnection(t, ctx, store, otherReplica.UID, stopped)

	store.SetInstanceID("instance-a")
	registerInstanceAs(t, ctx, store, "instance-b", 0)
	require.NoError(t, store.RegisterInstance(ctx))

	closed, err := store.CloseOrphanedConnections(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), closed.Own, "only instance-a's still-open connection should be reconciled")
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

	// openConnectionFor opens a connection stamped with someone else's instance
	// id, backdated so it looks like a session that stopped talking long ago.
	openConnectionFor := func(instanceID, sourceIP string) uuid.UUID {
		t.Helper()

		previous := store.InstanceID()
		store.SetInstanceID(instanceID)

		conn, err := store.CreateConnection(ctx, user.UID, database.UID, sourceIP)
		require.NoError(t, err)

		store.SetInstanceID(previous)
		backdateConnection(t, ctx, store, conn.UID, stopped)

		return conn.UID
	}

	// A replica that is up and heartbeating. Its sessions must survive: closing
	// them would let the retention sweep delete a connection a live session is
	// still writing queries against.
	registerInstanceAs(t, ctx, store, "live-instance", 0)
	liveConn := openConnectionFor("live-instance", "10.1.0.1")

	// A replica that registered and then stopped heartbeating — a crashed pod.
	registerInstanceAs(t, ctx, store, "stale-instance", InstanceStaleAfter+time.Minute)
	staleConn := openConnectionFor("stale-instance", "10.1.0.2")

	// A replica that shut down cleanly and deleted its registration. No row at
	// all, so it is gone immediately rather than after the grace period.
	registerInstanceAs(t, ctx, store, "gone-instance", 0)
	goneConn := openConnectionFor("gone-instance", "10.1.0.3")
	store.SetInstanceID("gone-instance")
	require.NoError(t, store.DeregisterInstance(ctx))

	// A legacy row from before the instance_id column existed. Deliberately
	// folded into the "no registry row" case: '' is never a live owner, since
	// no process can register it.
	legacyConn := openConnectionFor("", "10.1.0.4")

	// And this instance's own leftover, which is counted separately.
	store.SetInstanceID("reclaimer")
	require.NoError(t, store.RegisterInstance(ctx))

	ownConn := openConnectionFor("reclaimer", "10.1.0.5")

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

	openConnectionFor := func(instanceID, sourceIP string) uuid.UUID {
		t.Helper()

		previous := store.InstanceID()
		store.SetInstanceID(instanceID)

		conn, err := store.CreateConnection(ctx, user.UID, database.UID, sourceIP)
		require.NoError(t, err)

		store.SetInstanceID(previous)
		backdateConnection(t, ctx, store, conn.UID, stopped)

		return conn.UID
	}

	// This process: registered, heartbeating, and serving a session of its own.
	// The periodic reclaim runs while that session is live, so leaving it alone
	// is the property that makes running this outside startup safe at all.
	store.SetInstanceID("running-instance")
	require.NoError(t, store.RegisterInstance(ctx))

	ownLive, err := store.CreateConnection(ctx, user.UID, database.UID, "10.2.0.1")
	require.NoError(t, err)

	// A peer that is up and heartbeating.
	registerInstanceAs(t, ctx, store, "live-peer", 0)
	peerConn := openConnectionFor("live-peer", "10.2.0.2")

	// A peer that crashed: its row is still there, but long past the grace
	// period. This is what only the periodic pass catches.
	registerInstanceAs(t, ctx, store, "crashed-peer", InstanceStaleAfter+time.Minute)
	crashedConn := openConnectionFor("crashed-peer", "10.2.0.3")

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), reclaimed, "only the crashed peer's connection should be reclaimed")

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

	t.Run("this run's own live connection is untouched", func(t *testing.T) {
		// The reason the own-instance half of the reconcile stays at startup:
		// away from startup, our own open rows are sessions we are serving.
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

		registerInstanceAs(t, ctx, store, "another-crashed-peer", InstanceStaleAfter+time.Minute)
		orphan := openConnectionFor("another-crashed-peer", "10.2.0.4")

		none, err := store.ReclaimDeadInstanceConnections(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), none, "a process with no identity judges nobody")

		got, err := store.GetConnectionByUID(ctx, orphan)
		require.NoError(t, err)
		assert.Nil(t, got.DisconnectedAt)
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

	orphan, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.9")
	require.NoError(t, err)

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
