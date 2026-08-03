package postgresql

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// newCopyTestStore spins up a throwaway PostgreSQL store. Only dbbat's own
// storage DB is needed here — the capture under test is synthesized, not
// proxied — so this stays out of the integration-tagged suite and runs under
// `make test`.
func newCopyTestStore(t *testing.T) *store.Store {
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

// newPersistingSession builds a session wired to a real store and the real
// shared row writer, ready to run logQuery.
func newPersistingSession(t *testing.T, dataStore *store.Store, name string) *Session {
	t.Helper()

	ctx := context.Background()

	user, err := dataStore.CreateUser(ctx, name, "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         name + "db",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "u",
		Password:     "p",
		SSLMode:      "disable",
		Protocol:     store.ProtocolPostgreSQL,
	}, key)
	require.NoError(t, err)

	conn, err := dataStore.CreateConnection(ctx, user.UID, db.UID, "10.0.0.9")
	require.NoError(t, err)

	writer := shared.NewRowWriter(dataStore, slog.Default())
	t.Cleanup(func() { writer.Close(context.Background()) })

	s := newTestSession("read")
	s.ctx = ctx
	s.store = dataStore
	s.connectionUID = conn.UID
	s.rowWriter = writer
	s.queryStorage.StoreResults = true
	s.queryStorage.MaxResultRows = 100
	s.queryStorage.MaxResultBytes = 1 << 20

	return s
}

// awaitQueryWithRows polls until the session's single query has been persisted
// with at least wantRows rows. Persistence is asynchronous by design.
func awaitQueryWithRows(t *testing.T, dataStore *store.Store, wantRows int) *store.QueryWithRows {
	t.Helper()

	ctx := context.Background()

	var found *store.QueryWithRows

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{})
		if err != nil || len(queries) == 0 {
			return false
		}

		withRows, err := dataStore.GetQueryWithRows(ctx, queries[0].UID)
		if err != nil {
			return false
		}

		if len(withRows.Rows) < wantRows {
			return false
		}

		found = withRows

		return true
	}, 10*time.Second, 50*time.Millisecond, "the COPY capture never reached the store")

	return found
}

// copyRowNames decodes the "name" column out of persisted rows.
func copyRowNames(t *testing.T, rows []store.QueryRow) []string {
	t.Helper()

	names := make([]string, 0, len(rows))

	for _, row := range rows {
		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(row.RowData, &decoded))

		name, ok := decoded["name"].(string)
		require.True(t, ok, "name column should decode as a string")
		names = append(names, name)
	}

	return names
}

// TestCopyCapture_RowsArePersisted is the regression guard for the seam a COPY
// takes through the row writer: a COPY emits CopyData, never a DataRow, so
// nothing on the capture path ever opens a sink for it. logQuery has to open
// one at query end, or the rows silently go nowhere and the UI shows a
// finished COPY with zero rows.
func TestCopyCapture_RowsArePersisted(t *testing.T) {
	t.Parallel()

	dataStore := newCopyTestStore(t)
	s := newPersistingSession(t, dataStore, "copyfull")

	s.currentQuery = &pendingQuery{
		sql:       "COPY public.people (id, name) TO stdout",
		startTime: time.Now(),
	}
	s.copyState = &copyState{
		direction:   "out",
		format:      0,
		columnNames: []string{"id", "name"},
		dataChunks:  [][]byte{[]byte("1\talice\n2\tbob\n3\tcarol\n")},
	}

	s.logQuery(nil, nil, 128)

	persisted := awaitQueryWithRows(t, dataStore, 3)

	assert.Equal(t, []string{"alice", "bob", "carol"}, copyRowNames(t, persisted.Rows))
	assert.False(t, persisted.ResultsTruncated, "a complete COPY capture is not truncated")
	assert.False(t, persisted.ResultsDropped, "nothing should have been dropped")

	for i, row := range persisted.Rows {
		assert.Equal(t, i+1, row.RowNumber, "COPY rows keep their 1-based numbering")
	}

	require.NotNil(t, persisted.DurationMs, "the query must still be completed after the rows land")

	// Not asserted: copy_direction / copy_format. store.CreateQuery has never
	// copied them onto the row it inserts, so they are lost for every protocol
	// — a pre-existing gap tracked in
	// specs/todos/2026-08-03-create-query-drops-copy-metadata.md.
}

// TestCopyCapture_TruncatedPrefixIsPersisted covers the interaction with the
// byte-limit fix (spec 2026-08-03-copy-byte-limit-discards-captured-chunks):
// the prefix buffered before the limit must still reach the store, flagged as
// truncated and NOT as dropped.
func TestCopyCapture_TruncatedPrefixIsPersisted(t *testing.T) {
	t.Parallel()

	dataStore := newCopyTestStore(t)
	s := newPersistingSession(t, dataStore, "copytrunc")
	s.queryStorage.MaxResultBytes = 16

	s.currentQuery = &pendingQuery{
		sql:       "COPY public.people (id, name) TO stdout",
		startTime: time.Now(),
	}
	s.copyState = &copyState{
		direction:   "out",
		format:      0,
		columnNames: []string{"id", "name"},
	}

	s.captureCopyData([]byte("1\talice\n2\tbob\n"))
	s.captureCopyData([]byte("3\tcarol\n4\tdave\n"))

	require.True(t, s.copyState.truncated, "the byte limit must flag the capture as truncated")

	s.logQuery(nil, nil, 128)

	persisted := awaitQueryWithRows(t, dataStore, 2)

	assert.Equal(t, []string{"alice", "bob"}, copyRowNames(t, persisted.Rows),
		"the prefix captured before the limit must still be stored")
	assert.True(t, persisted.ResultsTruncated, "a byte-limited capture is truncated")
	assert.False(t, persisted.ResultsDropped, "a configured limit is never a drop")
}
