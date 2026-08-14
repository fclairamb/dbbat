package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionAuditEntries returns the session audit entries of one connection, in
// chain order, decoded.
func sessionAuditEntries(
	t *testing.T, ctx context.Context, store *Store, eventType string, connectionUID uuid.UUID,
) []connectionAuditDetails {
	t.Helper()

	var rows []AuditLog

	err := store.db.NewSelect().
		Model(&rows).
		Where("event_type = ?", eventType).
		Where("details ->> 'connection_uid' = ?", connectionUID.String()).
		Order("uid ASC").
		Scan(ctx)
	require.NoError(t, err, "reading the session audit entries")

	decoded := make([]connectionAuditDetails, 0, len(rows))

	for _, row := range rows {
		var details connectionAuditDetails

		require.NoError(t, json.Unmarshal(row.Details, &details), "decoding the session audit details")

		decoded = append(decoded, details)
	}

	return decoded
}

// TestSessionAuditOutlivesAWholeSessionDelete is the point of the whole feature:
// `DELETE FROM connections` takes the session, its statements and its captured
// rows with it, and used to leave nothing behind. The chained audit entries live
// in a table that delete does not touch — and deleting the connection must not
// disturb the audit chain either, because they are ordinary chained rows.
func TestSessionAuditOutlivesAWholeSessionDelete(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := createChainTestConnection(t, ctx, store, "wiped", 3)
	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	closed, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, closed.QueryChainMAC, "a session that logged statements is stamped on close")

	// The attack: one statement, and the session is gone.
	_, err = store.db.ExecContext(ctx, "DELETE FROM connections WHERE uid = ?", conn.UID)
	require.NoError(t, err)

	_, err = store.GetConnectionByUID(ctx, conn.UID)
	require.ErrorIs(t, err, ErrConnectionNotFound, "the row really is gone")

	var survivingStatements int

	require.NoError(t, store.db.NewSelect().
		Model((*Query)(nil)).
		ColumnExpr("count(*)").
		Where("connection_id = ?", conn.UID).
		Scan(ctx, &survivingStatements))
	require.Zero(t, survivingStatements, "the statements cascaded with the connection row")

	opened := sessionAuditEntries(t, ctx, store, AuditEventConnectionOpened, conn.UID)
	require.Len(t, opened, 1, "the session open must still be on record")

	assert.Equal(t, conn.UserID.String(), opened[0].UserID)
	assert.Equal(t, conn.DatabaseID.String(), opened[0].DatabaseID)
	assert.Equal(t, "10.0.0.1", opened[0].SourceIP)
	assert.Equal(t, auditTimestamp(conn.ConnectedAt), opened[0].ConnectedAt)
	assert.Empty(t, opened[0].DisconnectedAt, "an open entry describes an open session")

	gone := sessionAuditEntries(t, ctx, store, AuditEventConnectionClosed, conn.UID)
	require.Len(t, gone, 1, "the session close must still be on record")

	assert.Equal(t, connectionClosedBySession, gone[0].ClosedBy)
	assert.Equal(t, hex.EncodeToString(closed.QueryChainMAC), gone[0].QueryChainMAC,
		"the sealed record must point at the query chain the session owned")
	require.NotNil(t, gone[0].QueryChainLen)
	assert.Equal(t, int64(3), *gone[0].QueryChainLen)

	// And the deletion must not have broken anything: these are ordinary
	// chained audit rows, and nothing in `connections` is part of that chain.
	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	assert.True(t, result.OK(), "the audit chain must still verify: %v", result.Break)
}

// TestSessionAuditCleanCloseWritesOneEntry pins the clean-teardown writer: one
// open entry, one close entry, and a second CloseConnection — which closes
// nothing — writes no second one.
func TestSessionAuditCleanCloseWritesOneEntry(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "clean_close_audit")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.1.2.3")
	require.NoError(t, err)

	require.Len(t, sessionAuditEntries(t, ctx, store, AuditEventConnectionOpened, conn.UID), 1)
	require.Empty(t, sessionAuditEntries(t, ctx, store, AuditEventConnectionClosed, conn.UID),
		"a live session has no close entry")

	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	require.NotNil(t, row.DisconnectedAt)

	closed := sessionAuditEntries(t, ctx, store, AuditEventConnectionClosed, conn.UID)
	require.Len(t, closed, 1)

	assert.Equal(t, conn.UID.String(), closed[0].ConnectionUID)
	assert.Equal(t, user.UID.String(), closed[0].UserID)
	assert.Equal(t, database.UID.String(), closed[0].DatabaseID)
	assert.Equal(t, "10.1.2.3", closed[0].SourceIP,
		"the close entry must record the address the open entry did, mask-free")
	assert.Empty(t, closed[0].GrantUID, "this session authenticated under no grant")
	assert.Equal(t, store.InstanceID(), closed[0].InstanceID)
	assert.Equal(t, store.RunID(), closed[0].RunID)
	assert.Equal(t, connectionClosedBySession, closed[0].ClosedBy)
	assert.Equal(t, auditTimestamp(*row.DisconnectedAt), closed[0].DisconnectedAt)
	// A session that logged nothing carries no stamp, so the entry claims none.
	assert.Empty(t, closed[0].QueryChainMAC)
	assert.Nil(t, closed[0].QueryChainLen)

	// Closing an already-closed session updates no row, so it records nothing.
	require.ErrorIs(t, store.CloseConnection(ctx, conn.UID), ErrConnectionNotFound)
	assert.Len(t, sessionAuditEntries(t, ctx, store, AuditEventConnectionClosed, conn.UID), 1,
		"a close that closed nothing must not write a second entry")

	// The audit entry is a chained row like any other.
	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	assert.True(t, result.OK(), "the audit chain must verify: %v", result.Break)
}

// TestSessionAuditReconcileWritesOneEntry covers the other close writer: a
// crash-orphaned session, closed by the reclaim. Its entry says so, and carries
// the stamp the reconcile sealed — which is written *after* the close, so this
// also pins that the entry is built from the row as it finally stands.
func TestSessionAuditReconcileWritesOneEntry(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "reconcile_audit")

	var orphan, cleanlyClosed *Connection

	asRun(t, store, "instance-dead", "run-dead", func() {
		var err error

		orphan, err = store.CreateConnection(ctx, user.UID, database.UID, "10.9.9.9")
		require.NoError(t, err)

		_, err = store.CreateQuery(ctx, &Query{ConnectionID: orphan.UID, SQLText: "SELECT 1"})
		require.NoError(t, err)

		cleanlyClosed, err = store.CreateConnection(ctx, user.UID, database.UID, "10.9.9.8")
		require.NoError(t, err)

		require.NoError(t, store.CloseConnection(ctx, cleanlyClosed.UID))
	})

	store.SetInstanceID("instance-live")
	store.SetRunID("run-live")
	require.NoError(t, store.RegisterInstance(ctx))

	reclaimed, err := store.ReclaimDeadInstanceConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), reclaimed, "only the still-open orphan is reclaimed")

	row, err := store.GetConnectionByUID(ctx, orphan.UID)
	require.NoError(t, err)
	require.NotNil(t, row.DisconnectedAt)
	require.NotNil(t, row.QueryChainMAC, "the reconcile seals the head it recovered")

	closed := sessionAuditEntries(t, ctx, store, AuditEventConnectionClosed, orphan.UID)
	require.Len(t, closed, 1)

	assert.Equal(t, connectionClosedByReconcile, closed[0].ClosedBy)
	assert.Equal(t, "10.9.9.9", closed[0].SourceIP)
	// The identity is the *session's*, not the reconciling process's.
	assert.Equal(t, "instance-dead", closed[0].InstanceID)
	assert.Equal(t, "run-dead", closed[0].RunID)
	assert.Equal(t, auditTimestamp(*row.DisconnectedAt), closed[0].DisconnectedAt)
	assert.Equal(t, hex.EncodeToString(row.QueryChainMAC), closed[0].QueryChainMAC)
	require.NotNil(t, closed[0].QueryChainLen)
	assert.Equal(t, int64(1), *closed[0].QueryChainLen)

	// The session that closed cleanly already had its entry; the reconcile must
	// not add a second one.
	alreadyClosed := sessionAuditEntries(t, ctx, store, AuditEventConnectionClosed, cleanlyClosed.UID)
	require.Len(t, alreadyClosed, 1, "a cleanly closed session gets exactly one close entry")
	assert.Equal(t, connectionClosedBySession, alreadyClosed[0].ClosedBy)

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	assert.True(t, result.OK(), "the audit chain must verify: %v", result.Break)
}

// TestSessionAuditFailureDoesNotFailTheSession pins the rule that keeps this
// feature from being a new way to lose a database session: an audit write that
// cannot land is logged, never returned.
func TestSessionAuditFailureDoesNotFailTheSession(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "audit_unwritable")

	// The bluntest possible write failure, on this test's private database.
	_, err := store.db.ExecContext(ctx, "DROP TABLE audit_log")
	require.NoError(t, err)

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.4.4.4")
	require.NoError(t, err, "a session must open even when its audit entry cannot be written")
	require.NotNil(t, conn)

	require.NoError(t, store.CloseConnection(ctx, conn.UID),
		"a session must close even when its audit entry cannot be written")

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)
	assert.NotNil(t, row.DisconnectedAt, "the close itself must have committed")
}

// TestSessionAuditSurvivesACancelledContext covers the ordinary shape of a
// session close: the client is gone, so the context that closes the connection
// is already canceled. An audit trail that only recorded polite disconnections
// would be worth very little.
func TestSessionAuditSurvivesACancelledContext(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "audit_canceled")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.5.5.5")
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	// The close itself needs a live context; the audit write must not.
	require.NoError(t, store.CloseConnection(ctx, conn.UID))
	store.recordConnectionOpened(canceled, conn)

	assert.Len(t, sessionAuditEntries(t, ctx, store, AuditEventConnectionOpened, conn.UID), 2,
		"a canceled caller context must not swallow the audit entry")
}

// TestListAuditEventsExcludesSessionEvents pins the volume call: the session
// entries stay out of an unfiltered listing so they cannot bury the
// control-plane changes an operator opens the audit page for, and are one filter
// away.
func TestListAuditEventsExcludesSessionEvents(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "audit_listing")

	require.NoError(t, store.LogAuditEvent(ctx, &AuditEvent{
		EventType: "grant.created",
		UserID:    &user.UID,
		Details:   json.RawMessage(`{}`),
	}))

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.6.6.6")
	require.NoError(t, err)
	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	events, err := store.ListAuditEvents(ctx, AuditFilter{Limit: 100})
	require.NoError(t, err)

	for _, event := range events {
		assert.NotContains(t, SessionAuditEventTypes, event.EventType,
			"an unfiltered listing must not carry session events")
	}

	// The migration's chain anchor is an audit_log row like any other, so a
	// fresh store lists it alongside the event just written.
	assert.Len(t, events, 2, "the control-plane events are still listed")

	opened := AuditEventConnectionOpened

	byType, err := store.ListAuditEvents(ctx, AuditFilter{EventType: &opened, Limit: 100})
	require.NoError(t, err)
	assert.Len(t, byType, 1, "naming a session event type returns it")

	all, err := store.ListAuditEvents(ctx, AuditFilter{Limit: 100, IncludeSessionEvents: true})
	require.NoError(t, err)
	assert.Len(t, all, 4, "the opt-in folds them back in")
}

// TestLogAuditEventsChainsABatch covers the batch append the reconcile leans on:
// several entries, one transaction, linked to each other in the order given.
func TestLogAuditEventsChainsABatch(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	batch := make([]*AuditEvent, 0, 4)

	for i := range 4 {
		batch = append(batch, &AuditEvent{
			EventType: "chain_test",
			Details:   json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
		})
	}

	require.NoError(t, store.LogAuditEvents(ctx, batch...))

	rows := readChainedAuditRows(t, ctx, store)
	require.Len(t, rows, 4)

	for i, row := range rows {
		require.NotNil(t, row.ChainSeq)
		assert.Equal(t, int64(i+1), *row.ChainSeq)
		assert.NotEmpty(t, row.MAC)

		if i > 0 {
			assert.Equal(t, rows[i-1].MAC, row.PrevMAC, "each entry must link to its predecessor")
		}
	}

	result, err := store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	require.True(t, result.OK(), "a batched append must verify: %v", result.Break)
	assert.Equal(t, int64(4), result.Verified)

	// An empty batch is a no-op rather than an error.
	require.NoError(t, store.LogAuditEvents(ctx))
	assert.Len(t, readChainedAuditRows(t, ctx, store), 4)
}

// TestSessionAuditRecordsTheStoredConnectedAt guards the small honesty
// requirement behind the entry: the timestamp it carries has to be the one the
// row stores, or a reader comparing the two would see a mismatch that means
// nothing.
func TestSessionAuditRecordsTheStoredConnectedAt(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "audit_connected_at")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.7.7.7")
	require.NoError(t, err)

	row, err := store.GetConnectionByUID(ctx, conn.UID)
	require.NoError(t, err)

	opened := sessionAuditEntries(t, ctx, store, AuditEventConnectionOpened, conn.UID)
	require.Len(t, opened, 1)

	recorded, err := time.Parse(time.RFC3339Nano, opened[0].ConnectedAt)
	require.NoError(t, err)
	assert.True(t, recorded.Equal(row.ConnectedAt.UTC()),
		"the entry's connected_at (%s) must be the value the row stores (%s)",
		recorded, row.ConnectedAt.UTC())
}
