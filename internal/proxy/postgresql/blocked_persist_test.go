package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

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

// TestBlockedStatements_AreOrdinaryChainAppends runs refusals through a store
// with the HMAC query chain switched on. `queries` is chained per connection,
// so a refusal row is only correct if it is an ordinary link in that chain —
// one append, in order, verifiable by `dbbat audit verify --queries`.
//
// It matters more here than on Oracle: the extended-query path answers a
// refusal in the middle of a batch the client already pipelined, which is
// exactly where a double append or an out-of-order one would show up. Both
// protocols therefore refuse on the *same* connection, so the two rows land in
// the same chain, one after the other.
func TestBlockedStatements_AreOrdinaryChainAppends(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	dataStore := newChainedTestStore(t)
	require.True(t, dataStore.ChainEnabled(), "the query chain must be on for this test to mean anything")

	s := newRefusingSession(t, dataStore, []string{store.ControlReadOnly})

	const (
		simpleSQL   = "INSERT INTO ro (id) VALUES (1)"
		extendedSQL = "UPDATE ro SET id = 2"
	)

	// Simple query protocol.
	require.ErrorIs(t, s.handleQuery(&pgproto3.Query{String: simpleSQL}), ErrWriteNotPermitted)

	// The first row has to be settled before the second is driven: the chain is
	// an ordered append and the writes are asynchronous, so racing them would
	// test the test, not the chain.
	first := awaitBlockedQueries(t, s, 1)
	assertBlockedRow(t, first[0], simpleSQL, ErrWriteNotPermitted)

	// Extended query protocol, with the Bind and Execute the client pipelined
	// behind the Parse that is about to be refused.
	require.ErrorIs(t, s.handleParse(&pgproto3.Parse{Name: "st", Query: extendedSQL}), ErrWriteNotPermitted)
	s.handleBind(&pgproto3.Bind{DestinationPortal: "", PreparedStatement: "st"})
	require.NoError(t, s.handleExecute(&pgproto3.Execute{Portal: ""}))

	rows := awaitBlockedQueries(t, s, 2)
	assertBlockedRow(t, rows[0], extendedSQL, ErrWriteNotPermitted)
	assertBlockedRow(t, rows[1], simpleSQL, ErrWriteNotPermitted)

	result, err := dataStore.VerifyQueryChain(ctx, s.connectionUID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a refusal row must be a valid link in the connection's query chain")
	assert.False(t, result.TruncatedPrefix, "nothing was retained away; the chain starts at seq 1")
	assert.Equal(t, int64(2), result.Verified, "both refusals must be sealed into the chain")
}

// newChainedTestStore is newCopyTestStore with the tamper-evident chain
// enabled — the store derives its HMAC subkey from the master key, so passing
// one is what makes ChainEnabled() true and chain_seq/mac get written.
func newChainedTestStore(t *testing.T) *store.Store {
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

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(0xA0 + i)
	}

	dataStore, err := store.New(ctx, dsn, store.Options{EncryptionKey: masterKey})
	require.NoError(t, err)
	t.Cleanup(func() { dataStore.Close() })

	require.NoError(t, dataStore.Migrate(ctx))

	return dataStore
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
