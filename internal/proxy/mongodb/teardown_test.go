package mongodb

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// newTeardownSession builds a session past authentication, wired to a real store,
// with no relay running: the teardown is called directly, in the state the
// relay leaves it in once both pumps have stopped.
func newTeardownSession(t *testing.T) (*Session, *store.Store) {
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
				WithStartupTimeout(120*time.Second),
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

	user, err := dataStore.CreateUser(ctx, "alice", "unused-hash", []string{store.RoleConnector})
	require.NoError(t, err)

	encKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i + 1)
	}

	database, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "orders",
		Host:         "127.0.0.1",
		Port:         27017,
		DatabaseName: "orders",
		Username:     "root",
		Password:     "secret",
		Protocol:     store.ProtocolMongoDB,
	}, encKey)
	require.NoError(t, err)

	conn, err := dataStore.CreateConnection(ctx, user.UID, database.UID, "127.0.0.1")
	require.NoError(t, err)

	srv, err := NewServer(dataStore, encKey, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.MongoConfig{}, slog.Default())
	require.NoError(t, err)

	proxySide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = proxySide.Close()
		_ = clientSide.Close()
	})

	s := newSession(proxySide, srv)
	s.ctx = ctx
	s.logger = slog.Default()
	s.connection = conn
	s.user = user
	s.database = database

	return s, dataStore
}

// closedConnectionQueries runs the teardown and returns the statements stored
// once it has returned, oldest first. No polling: the teardown is what must
// have written them before closing the connection row.
func closedConnectionQueries(t *testing.T, s *Session, dataStore *store.Store) []store.Query {
	t.Helper()

	s.recordDisconnect()

	ctx := context.Background()

	connections, err := dataStore.ListConnections(ctx, store.ConnectionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, connections, 1)
	require.NotNil(t, connections[0].DisconnectedAt, "the teardown must close the connection row")

	queries, err := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 10})
	require.NoError(t, err)

	// ListQueries is newest first.
	for i, j := 0, len(queries)-1; i < j; i, j = i+1, j-1 {
		queries[i], queries[j] = queries[j], queries[i]
	}

	return queries
}

// TestCommandsAwaitingAReplyAtClientDisconnectAreLogged is the MongoDB half of
// security scan finding M6: a forwarded command whose client hung up before the
// reply was dropped at teardown unless dbbat itself had ended the session.
// Plural, because a driver pipelines.
func TestCommandsAwaitingAReplyAtClientDisconnectAreLogged(t *testing.T) {
	t.Parallel()

	s, dataStore := newTeardownSession(t)

	start := time.Now().Add(-time.Second)
	s.registerPending(7, &pendingQuery{command: "delete", sqlText: `delete {"delete":"orders"}`, start: start})
	s.registerPending(3, &pendingQuery{
		command: "insert", sqlText: `insert {"insert":"orders"}`, start: start.Add(100 * time.Millisecond),
	})

	queries := closedConnectionQueries(t, s, dataStore)
	require.Len(t, queries, 2, "every command still in flight must be logged before the session record closes")

	assert.Equal(t, `delete {"delete":"orders"}`, queries[0].SQLText)
	assert.Equal(t, `insert {"insert":"orders"}`, queries[1].SQLText)

	for _, q := range queries {
		require.NotNil(t, q.Error)
		assert.Equal(t, errClientDisconnectedMidResponse, *q.Error)
		assert.Nil(t, q.RowsAffected)
	}

	assert.Empty(t, s.pending)
	assert.False(t, s.statementClock.Running(), "nothing is executing upstream any more")
}

// TestCommandAwaitingAReplyAtTerminationCarriesTheReason keeps the earlier
// behaviour: when dbbat ended the session, the reason is the error.
func TestCommandAwaitingAReplyAtTerminationCarriesTheReason(t *testing.T) {
	t.Parallel()

	s, dataStore := newTeardownSession(t)

	s.registerPending(1, &pendingQuery{command: "find", sqlText: `find {"find":"orders"}`, start: time.Now()})

	termination := store.Termination{Reason: store.TerminationAdminTerminated, By: "bob"}
	s.termination = termination

	queries := closedConnectionQueries(t, s, dataStore)
	require.Len(t, queries, 1)
	require.NotNil(t, queries[0].Error)
	assert.Equal(t, termination.Message(), *queries[0].Error)
}

// TestTeardownWithNothingInFlightLogsNothing is the positive control: a session
// whose every command was answered gains no row at teardown.
func TestTeardownWithNothingInFlightLogsNothing(t *testing.T) {
	t.Parallel()

	s, dataStore := newTeardownSession(t)

	assert.Empty(t, closedConnectionQueries(t, s, dataStore))
}
