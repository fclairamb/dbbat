package mysql

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/store"
)

// newTeardownTestStore spins up a throwaway PostgreSQL store for teardown-path
// tests. Only the dbbat storage DB is needed here — no MySQL upstream — so this
// stays out of the integration-tagged suite and runs under `make test`.
func newTeardownTestStore(t *testing.T) *store.Store {
	t.Helper()

	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	dataStore, err := store.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { dataStore.Close() })

	require.NoError(t, dataStore.Migrate(ctx))

	return dataStore
}

// TestRecordDisconnect_FlushesUnrecordedBytes asserts the MySQL teardown flushes
// client-side bytes that were never attributed to a completed query — an
// aborted query's partial request bytes or the last query's trailing response
// — into the connection's bytes_transferred, WITHOUT bumping the query count.
// Persisting them keeps the grant's recomputed BytesTransferred honest across
// reconnects (the core concern of spec 2026-07-14-09).
func TestRecordDisconnect_FlushesUnrecordedBytes(t *testing.T) {
	dataStore := newTeardownTestStore(t)
	ctx := context.Background()

	user, err := dataStore.CreateUser(ctx, "mysqlteardown", "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	db, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         "mysqlteardowndb",
		Host:         "localhost",
		Port:         3306,
		DatabaseName: "db",
		Username:     "u",
		Password:     "p",
		SSLMode:      "disable",
		Protocol:     "mysql",
	}, key)
	require.NoError(t, err)

	conn, err := dataStore.CreateConnection(ctx, user.UID, db.UID, "10.0.0.5")
	require.NoError(t, err)

	// A completed query already persisted 200 bytes (and 1 query) and advanced
	// the snapshot to 200.
	require.NoError(t, dataStore.IncrementConnectionStats(ctx, conn.UID, 200))

	// The live counters now stand at 1000 cumulative client-side bytes, so 800
	// bytes (trailing response / aborted-Execute request) remain unrecorded.
	var from, to atomic.Int64
	from.Store(300)
	to.Store(700)

	s := &Session{
		server:            &Server{store: dataStore, logger: discardLogger(), ctx: ctx},
		logger:            discardLogger(),
		ctx:               ctx,
		connection:        conn,
		bytesFromClient:   &from,
		bytesToClient:     &to,
		lastBytesSnapshot: 200,
	}

	s.recordDisconnect()

	conns, err := dataStore.ListConnections(ctx, store.ConnectionFilter{UserID: &user.UID})
	require.NoError(t, err)

	var found *store.Connection

	for i := range conns {
		if conns[i].UID == conn.UID {
			found = &conns[i]

			break
		}
	}

	require.NotNil(t, found, "connection not found")

	// 200 (completed query) + 800 (flushed teardown delta) = 1000.
	require.Equal(t, int64(1000), found.BytesTransferred,
		"teardown must flush the 800 unrecorded client-side bytes")
	// The bytes-only flush must NOT bump the query count.
	require.Equal(t, int64(1), found.Queries,
		"teardown byte flush must not inflate the query count")
	require.NotNil(t, found.DisconnectedAt, "recordDisconnect must also close the connection")
}
