package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createQueryWithRows inserts a query at the given time plus two captured
// result rows, so the cascade to query_rows can be observed.
func createQueryWithRows(
	t *testing.T,
	ctx context.Context,
	store *Store,
	connUID uuid.UUID,
	sqlText string,
	executedAt time.Time,
) *Query {
	t.Helper()

	query, err := store.CreateQuery(ctx, &Query{
		ConnectionID: connUID,
		SQLText:      sqlText,
		ExecutedAt:   executedAt,
	})
	require.NoError(t, err)

	rows := []QueryRow{
		{RowNumber: 1, RowData: json.RawMessage(`{"a":1}`), RowSizeBytes: 7},
		{RowNumber: 2, RowData: json.RawMessage(`{"a":2}`), RowSizeBytes: 7},
	}
	require.NoError(t, store.StoreQueryRows(ctx, pendingRows(query.UID, rows)))

	return query
}

func countQueryRows(t *testing.T, ctx context.Context, store *Store, queryUID uuid.UUID) int {
	t.Helper()

	count, err := store.db.NewSelect().
		Model((*QueryRowModel)(nil)).
		Where("query_id = ?", queryUID).
		Count(ctx)
	require.NoError(t, err)

	return count
}

func TestCleanupOldQueryRows(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "retention_basic")

	old := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'old'", time.Now().Add(-48*time.Hour))
	recent := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'recent'", time.Now().Add(-1*time.Hour))

	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Queries, "only the query older than the cutoff should be deleted")
	assert.Equal(t, int64(0), result.Connections, "an open connection must never be reaped")

	// (a) rows older than the cutoff go away
	_, err = store.GetQuery(ctx, old.UID)
	require.ErrorIs(t, err, ErrQueryNotFound, "old query should be gone")

	// (c) the cascade removed the dependent query_rows
	assert.Equal(t, 0, countQueryRows(t, ctx, store, old.UID), "query_rows should cascade away with the query")

	// (b) rows newer than the cutoff survive, with their captured rows
	survivor, err := store.GetQuery(ctx, recent.UID)
	require.NoError(t, err)
	assert.Equal(t, "SELECT 'recent'", survivor.SQLText)
	assert.Equal(t, 2, countQueryRows(t, ctx, store, recent.UID))

	// The open connection outlives its reaped queries.
	stillThere, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	assert.Equal(t, conn.UID, stillThere.UID)
}

func TestCleanupOldQueryRowsDisabled(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "retention_off")
	old := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'ancient'", time.Now().Add(-3000*time.Hour))

	for _, retention := range []time.Duration{0, -time.Hour} {
		result, err := store.CleanupOldQueryRows(ctx, retention, retention)
		require.NoError(t, err)
		assert.Equal(t, RetentionSweepResult{}, result, "retention %s must be a no-op", retention)
	}

	_, err := store.GetQuery(ctx, old.UID)
	require.NoError(t, err, "disabled retention must keep history forever")
	assert.Equal(t, 2, countQueryRows(t, ctx, store, old.UID))
}

// TestCleanupOldQueryRowsReapsClosedConnections documents the connection
// lifetime decision: connections closed before the cutoff are removed (taking
// their queries and rows with them), while connections still open are kept
// whatever their age.
func TestCleanupOldQueryRowsReapsClosedConnections(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "retention_conns")

	closedConn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	closedQuery := createQueryWithRows(t, ctx, store, closedConn.UID, "SELECT 'closed'", time.Now().Add(-48*time.Hour))

	// Backdate the disconnect: CloseConnection always stamps "now".
	long := time.Now().Add(-48 * time.Hour)
	_, err = store.db.ExecContext(ctx,
		"UPDATE connections SET disconnected_at = ?, last_activity_at = ? WHERE uid = ?", long, long, closedConn.UID)
	require.NoError(t, err)

	// An open connection of the same age must survive.
	openConn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx,
		"UPDATE connections SET connected_at = ?, last_activity_at = ? WHERE uid = ?", long, long, openConn.UID)
	require.NoError(t, err)

	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Connections)
	assert.Equal(t, int64(0), result.Queries, "the query went away through the connection cascade")

	_, err = store.GetConnectionByUID(ctx, closedConn.UID)
	require.Error(t, err, "closed connection older than the cutoff should be gone")

	_, err = store.GetQuery(ctx, closedQuery.UID)
	require.ErrorIs(t, err, ErrQueryNotFound, "queries cascade from the connection")
	assert.Equal(t, 0, countQueryRows(t, ctx, store, closedQuery.UID))

	_, err = store.GetConnectionByUID(ctx, openConn.UID)
	require.NoError(t, err, "an open connection must be kept whatever its age")
}

// TestCleanupOldQueryRowsBatches proves the batched delete loop terminates and
// removes everything when there are more matching rows than fit in one batch.
func TestCleanupOldQueryRowsBatches(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "retention_batch")

	// One and a half batches, so the loop must run more than once.
	const total = RetentionBatchSize + RetentionBatchSize/2

	_, err := store.db.ExecContext(ctx,
		`INSERT INTO queries (uid, connection_id, sql_text, executed_at)
		 SELECT gen_random_uuid(), ?, 'SELECT ' || g, NOW() - INTERVAL '48 hours'
		 FROM generate_series(1, ?) g`, conn.UID, total)
	require.NoError(t, err)

	type sweep struct {
		result RetentionSweepResult
		err    error
	}

	done := make(chan sweep, 1)

	go func() {
		result, sweepErr := store.CleanupOldQueryRows(ctx, 24*time.Hour, 24*time.Hour)
		done <- sweep{result, sweepErr}
	}()

	select {
	case s := <-done:
		require.NoError(t, s.err)
		assert.Equal(t, int64(total), s.result.Queries)
	case <-time.After(60 * time.Second):
		t.Fatal("batched retention sweep did not terminate")
	}

	remaining, err := store.db.NewSelect().
		Model((*Query)(nil)).
		Where("connection_id = ?", conn.UID).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, remaining)
}

// TestCleanupOldQueryRowsTwoWindows is the split: statements expire on their
// own, shorter window while the session ledger they hang off is kept on a much
// longer one. A session closed 40 days ago under (30d statements, 365d
// sessions) keeps its row and loses every statement — the state that used to be
// reachable only for an *open* session, and that is now the ordinary condition
// of every closed session between the two windows.
func TestCleanupOldQueryRowsTwoWindows(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	const (
		queryWindow      = 30 * 24 * time.Hour
		connectionWindow = 365 * 24 * time.Hour
	)

	user, database := createTestUserAndDatabase(t, ctx, store, "retention_two_windows")

	// Closed 40 days ago: past the statement window, well inside the ledger one.
	middle, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	middleQuery := createQueryWithRows(t, ctx, store, middle.UID, "SELECT 'middle'", time.Now().Add(-40*24*time.Hour))
	closeConnectionAgo(t, ctx, store, middle.UID, 40*24*time.Hour)

	// Closed 400 days ago: past both windows, so it goes entirely.
	ancient, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.2")
	require.NoError(t, err)
	ancientQuery := createQueryWithRows(t, ctx, store, ancient.UID, "SELECT 'ancient'", time.Now().Add(-400*24*time.Hour))
	closeConnectionAgo(t, ctx, store, ancient.UID, 400*24*time.Hour)

	// Open, and older than both windows: never reaped, whatever the windows say.
	open, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.3")
	require.NoError(t, err)
	openQuery := createQueryWithRows(t, ctx, store, open.UID, "SELECT 'open'", time.Now().Add(-500*24*time.Hour))

	_, err = store.db.ExecContext(ctx,
		"UPDATE connections SET connected_at = ?, last_activity_at = ? WHERE uid = ?",
		time.Now().Add(-500*24*time.Hour), time.Now().Add(-500*24*time.Hour), open.UID)
	require.NoError(t, err)

	result, err := store.CleanupOldQueryRows(ctx, queryWindow, connectionWindow)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Connections, "only the session past the ledger window is reaped whole")
	assert.Equal(t, int64(2), result.Queries,
		"the statements of the surviving sessions — closed and open alike — are past the statement window")

	// The 40-day-old session keeps its ledger row and loses its statements.
	ledger, err := store.GetConnectionByUID(ctx, middle.UID)
	require.NoError(t, err)
	assert.Equal(t, middle.UID, ledger.UID)

	_, err = store.GetQuery(ctx, middleQuery.UID)
	require.ErrorIs(t, err, ErrQueryNotFound,
		"a closed connection can now outlive all of its statements")
	assert.Equal(t, 0, countQueryRows(t, ctx, store, middleQuery.UID))

	// The 400-day-old one is gone outright, statements included.
	_, err = store.GetConnectionByUID(ctx, ancient.UID)
	require.Error(t, err, "a session past the ledger window is deleted")

	_, err = store.GetQuery(ctx, ancientQuery.UID)
	require.ErrorIs(t, err, ErrQueryNotFound)

	// The open one keeps its row, as always.
	_, err = store.GetConnectionByUID(ctx, open.UID)
	require.NoError(t, err, "an open connection is never reaped, whatever its age")

	_, err = store.GetQuery(ctx, openQuery.UID)
	require.ErrorIs(t, err, ErrQueryNotFound, "its statements still expire on the statement window")
}

// TestCleanupOldQueryRowsLedgerKeptForever covers the other interesting
// combination the split makes expressible: DBB_CONNECTION_RETENTION=0 with a
// statement window set. Statements expire; no session is ever deleted.
func TestCleanupOldQueryRowsLedgerKeptForever(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "retention_ledger_forever")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	query := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'old'", time.Now().Add(-3000*time.Hour))
	closeConnectionAgo(t, ctx, store, conn.UID, 3000*time.Hour)

	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.Connections, "a zero ledger window keeps every session forever")
	assert.Equal(t, int64(1), result.Queries)

	_, err = store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)

	_, err = store.GetQuery(ctx, query.UID)
	require.ErrorIs(t, err, ErrQueryNotFound)
}

// TestCleanupOldQueryRowsEqualWindows pins the backward-compatibility promise:
// with both windows equal — which is what an unset DBB_CONNECTION_RETENTION
// resolves to — the sweep produces exactly the counts it produced before the
// split.
func TestCleanupOldQueryRowsEqualWindows(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "retention_equal")

	closed, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	require.NoError(t, err)
	createQueryWithRows(t, ctx, store, closed.UID, "SELECT 'closed'", time.Now().Add(-48*time.Hour))
	closeConnectionAgo(t, ctx, store, closed.UID, 48*time.Hour)

	openConn := createTestConnection(t, ctx, store, "retention_equal_open")
	createQueryWithRows(t, ctx, store, openConn.UID, "SELECT 'open'", time.Now().Add(-48*time.Hour))

	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Connections, "the closed session goes whole, cascading its statement")
	assert.Equal(t, int64(1), result.Queries, "the open session's statement is deleted on its own")

	_, err = store.GetConnectionByUID(ctx, closed.UID)
	require.Error(t, err)

	_, err = store.GetConnectionByUID(ctx, openConn.UID)
	require.NoError(t, err)
}

// closeConnectionAgo marks a connection as having both started and ended that
// long ago, so a retention sweep sees a session that ran and ended in the past.
// CreateConnection and CloseConnection both stamp "now".
func closeConnectionAgo(t *testing.T, ctx context.Context, store *Store, uid uuid.UUID, ago time.Duration) {
	t.Helper()

	when := time.Now().Add(-ago)

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET connected_at = ?, disconnected_at = ?, last_activity_at = ? WHERE uid = ?`,
		when, when, when, uid)
	require.NoError(t, err)
}
