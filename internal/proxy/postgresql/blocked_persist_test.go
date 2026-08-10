package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// awaitBlockedQueries polls until the connection has exactly `want` query rows
// and returns them. Persistence is asynchronous by design, so this waits for
// the row rather than reading straight after the refusal.
//
// It deliberately asserts on the *count* as well: the extended-protocol case
// exists precisely because a refused Parse used to be able to leave a second,
// empty row behind.
func awaitBlockedQueries(t *testing.T, s *Session, want int) []store.Query {
	t.Helper()

	ctx := context.Background()
	connUID := s.connectionUID

	var found []store.Query

	require.Eventually(t, func() bool {
		queries, err := s.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &connUID, Limit: 50})
		if err != nil || len(queries) != want {
			return false
		}

		found = queries

		return true
	}, 10*time.Second, 50*time.Millisecond, "expected %d persisted query row(s) for the refused statement", want)

	// Give a stray extra row a chance to show up before declaring the count
	// correct — otherwise "exactly one" only means "one so far".
	time.Sleep(250 * time.Millisecond)

	queries, err := s.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &connUID, Limit: 50})
	require.NoError(t, err)
	require.Len(t, queries, want, "no extra query row must be written for a single refused statement")

	return found
}

// assertBlockedRow checks the shape every refusal row shares, on all five
// protocols: the statement text as attempted, a non-null error, and a
// zero duration / zero rows_affected because the statement never ran.
func assertBlockedRow(t *testing.T, row store.Query, wantSQL string, wantErr error) {
	t.Helper()

	assert.Equal(t, wantSQL, row.SQLText)
	require.NotNil(t, row.Error, "a refused statement must be persisted with its refusal as error")
	assert.Equal(t, wantErr.Error(), *row.Error)
	require.NotNil(t, row.DurationMs)
	assert.InDelta(t, 0, *row.DurationMs, 0.001, "a refused statement never ran")
	require.NotNil(t, row.RowsAffected)
	assert.Equal(t, int64(0), *row.RowsAffected)
}

// TestBlockedStatements_ArePersisted is the store-backed proof for spec
// 2026-08-09-log-blocked-statements-pg-oracle: a statement refused by
// read_only, block_copy or block_ddl leaves a queries row with the refusal as
// `error`, on both the simple-query and the extended-query path.
//
// One storage container is shared by every case; each case gets its own
// connection row so the assertions can be scoped to it.
func TestBlockedStatements_ArePersisted(t *testing.T) {
	t.Parallel()

	dataStore := newCopyTestStore(t)

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  error
	}{
		{
			name:     "read_only refuses a write",
			controls: []string{store.ControlReadOnly},
			sql:      "INSERT INTO ro (id) VALUES (1)",
			wantErr:  ErrWriteNotPermitted,
		},
		{
			name:     "block_ddl refuses a CREATE TABLE",
			controls: []string{store.ControlBlockDDL},
			sql:      "CREATE TABLE blocked_ddl (id int)",
			wantErr:  ErrDDLNotPermitted,
		},
		{
			name:     "block_copy refuses a COPY",
			controls: []string{store.ControlBlockCopy},
			sql:      "COPY cp TO STDOUT",
			wantErr:  ErrCopyNotPermitted,
		},
		{
			name:     "read_only refuses a bypass attempt",
			controls: []string{store.ControlReadOnly},
			sql:      "SET SESSION default_transaction_read_only = off",
			wantErr:  ErrReadOnlyBypassAttempt,
		},
	}

	for _, tt := range tests {
		t.Run("simple/"+tt.name, func(t *testing.T) {
			t.Parallel()

			s := newRefusingSession(t, dataStore, tt.controls)

			require.ErrorIs(t, s.handleQuery(&pgproto3.Query{String: tt.sql}), tt.wantErr)

			rows := awaitBlockedQueries(t, s, 1)
			assertBlockedRow(t, rows[0], tt.sql, tt.wantErr)
		})

		t.Run("extended/"+tt.name, func(t *testing.T) {
			t.Parallel()

			s := newRefusingSession(t, dataStore, tt.controls)

			require.ErrorIs(t, s.handleParse(&pgproto3.Parse{Name: "st", Query: tt.sql}), tt.wantErr)

			rows := awaitBlockedQueries(t, s, 1)
			assertBlockedRow(t, rows[0], tt.sql, tt.wantErr)
		})
	}
}

// TestBlockedParse_DoesNotDoubleRecordOnExecute covers the extended-protocol
// hazard the spec calls out: a client that pipelines Parse/Bind/Execute has
// already sent the Bind and the Execute by the time the Parse is refused. The
// refusal must be recorded exactly once, and the Execute that follows must not
// add a second (empty-SQL) row.
//
// The handlers are driven directly here — the transport-level guard that stops
// those messages from ever reaching them is covered by
// TestDiscardUntilSync_DropsTheRestOfTheBatch.
func TestBlockedParse_DoesNotDoubleRecordOnExecute(t *testing.T) {
	t.Parallel()

	dataStore := newCopyTestStore(t)
	s := newRefusingSession(t, dataStore, []string{store.ControlReadOnly})

	const sql = "UPDATE ro SET id = 2"

	require.ErrorIs(t, s.handleParse(&pgproto3.Parse{Name: "st", Query: sql}), ErrWriteNotPermitted)

	// The rest of the pipelined batch, as the client sent it.
	s.handleBind(&pgproto3.Bind{DestinationPortal: "", PreparedStatement: "st"})
	require.NoError(t, s.handleExecute(&pgproto3.Execute{Portal: ""}))

	rows := awaitBlockedQueries(t, s, 1)
	assertBlockedRow(t, rows[0], sql, ErrWriteNotPermitted)
}

// newRefusingSession builds a store-backed session whose grant carries the
// given controls.
func newRefusingSession(t *testing.T, dataStore *store.Store, controls []string) *Session {
	t.Helper()

	// Users and servers are unique by name in the store, and every case here
	// needs its own connection row to scope its assertions to.
	s := newPersistingSession(t, dataStore, "blocked"+uuid.NewString()[:8])
	s.grant = &store.Grant{Definition: &store.GrantDefinition{Controls: controls}}

	return s
}

// TestDiscardUntilSync_DropsTheRestOfTheBatch covers the extended-protocol
// error state dbbat now mirrors: once a message in a batch is refused, every
// following extended message is swallowed (never forwarded upstream, never
// re-entering the handlers) until the client's Sync, which resets the state and
// *is* forwarded so upstream answers it with the ReadyForQuery the client is
// waiting for.
func TestDiscardUntilSync_DropsTheRestOfTheBatch(t *testing.T) {
	t.Parallel()

	s := newTestSession("read")
	s.extendedState.errorUntilSync = true

	dropped := []pgproto3.FrontendMessage{
		&pgproto3.Bind{},
		&pgproto3.Describe{},
		&pgproto3.Execute{},
		&pgproto3.Close{},
		&pgproto3.Flush{},
		&pgproto3.Parse{},
		&pgproto3.CopyData{},
		&pgproto3.CopyDone{},
		&pgproto3.CopyFail{},
	}

	for _, msg := range dropped {
		assert.Truef(t, s.discardUntilSync(msg), "%T must be discarded while the batch is failed", msg)
		assert.True(t, s.extendedState.errorUntilSync, "the error state lasts until Sync")
	}

	assert.False(t, s.discardUntilSync(&pgproto3.Sync{}), "Sync is forwarded, not discarded")
	assert.False(t, s.extendedState.errorUntilSync, "Sync clears the error state")

	// Outside the error state nothing is discarded.
	assert.False(t, s.discardUntilSync(&pgproto3.Bind{}))
}

// TestIsExtendedQueryMessage tells the two protocols apart, which is what
// decides whether a refusal answers with ReadyForQuery immediately (simple) or
// leaves it to the client's Sync (extended).
func TestIsExtendedQueryMessage(t *testing.T) {
	t.Parallel()

	assert.True(t, isExtendedQueryMessage(&pgproto3.Parse{}))
	assert.True(t, isExtendedQueryMessage(&pgproto3.Bind{}))
	assert.True(t, isExtendedQueryMessage(&pgproto3.Describe{}))
	assert.True(t, isExtendedQueryMessage(&pgproto3.Execute{}))
	assert.True(t, isExtendedQueryMessage(&pgproto3.Close{}))
	assert.False(t, isExtendedQueryMessage(&pgproto3.Query{}))
	assert.False(t, isExtendedQueryMessage(&pgproto3.Sync{}))
	assert.False(t, isExtendedQueryMessage(&pgproto3.Terminate{}))
}
