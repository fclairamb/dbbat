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
	require.NoError(t, store.StoreQueryRows(ctx, PendingRows(query.UID, rows)))

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
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "retention-basic")

	old := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'old'", time.Now().Add(-48*time.Hour))
	recent := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'recent'", time.Now().Add(-1*time.Hour))

	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
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
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "retention-off")
	old := createQueryWithRows(t, ctx, store, conn.UID, "SELECT 'ancient'", time.Now().Add(-3000*time.Hour))

	for _, retention := range []time.Duration{0, -time.Hour} {
		result, err := store.CleanupOldQueryRows(ctx, retention)
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
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "retention-conns")

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

	result, err := store.CleanupOldQueryRows(ctx, 24*time.Hour)
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
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "retention-batch")

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
		result, sweepErr := store.CleanupOldQueryRows(ctx, 24*time.Hour)
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
