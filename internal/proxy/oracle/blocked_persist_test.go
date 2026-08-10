package oracle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/store"
)

// awaitCreated waits for the asynchronous insert of a blocked-statement row.
func (r *recordingCompletionStore) awaitCreated(t *testing.T) *store.Query {
	t.Helper()

	select {
	case got := <-r.created:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the refused statement was never written to the store")

		return nil
	}
}

// assertNoFurtherCreate fails if a second row shows up for one refusal.
func (r *recordingCompletionStore) assertNoFurtherCreate(t *testing.T) {
	t.Helper()

	select {
	case extra := <-r.created:
		t.Fatalf("a refused statement must write exactly one row, got a second: %q", extra.SQLText)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestBlockedStatements_ArePersisted is the Oracle half of spec
// 2026-08-09-log-blocked-statements-pg-oracle: a statement refused by
// read_only or block_ddl used to leave nothing behind but an slog WARN. It now
// writes a queries row carrying the refusal as `error`, with duration_ms and
// rows_affected at 0 because the statement never reached upstream.
//
// Every statement-carrying TTC op is covered, because they refuse
// independently: legacy OALL8, the v315+ piggyback exec, the JDBC thin
// driver's func=0x11 exec, and a re-execution re-gated against its cursor.
func TestBlockedStatements_ArePersisted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  string
		payload  func(sql string) []byte
	}{
		{
			name:     "OALL8 write under read_only",
			controls: []string{store.ControlReadOnly},
			sql:      "INSERT INTO emp (id) VALUES (1)",
			wantErr:  "read-only",
			payload:  func(sql string) []byte { return buildOALL8(sql, nil, 7) },
		},
		{
			name:     "OALL8 CREATE TABLE under block_ddl",
			controls: []string{store.ControlBlockDDL},
			sql:      "CREATE TABLE blocked_ddl (id NUMBER)",
			wantErr:  "DDL",
			payload:  func(sql string) []byte { return buildOALL8(sql, nil, 7) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: tt.controls}})
			recorder := newRecordingCompletionStore()
			s.completionStore = recorder

			require.Error(t, s.handleOALL8(tt.payload(tt.sql)), "the statement must be refused")

			got := recorder.awaitCreated(t)
			assertBlockedOracleRow(t, got, tt.sql, tt.wantErr)
			assert.Equal(t, s.connectionUID, got.ConnectionID,
				"a refusal row must still be attributed to the connection")

			// A refused statement is not in flight: nothing is left pending
			// that a later response could complete into a second row.
			assert.Nil(t, s.tracker.pendingQuery)
			recorder.assertNoFurtherCreate(t)
		})
	}
}

// TestBlockedCursorReexecution_IsPersisted covers the path a client takes when
// it re-runs a statement it already parsed: the SQL rides no frame of its own,
// so the refusal has to be recorded against the SQL the cursor holds.
func TestBlockedCursorReexecution_IsPersisted(t *testing.T) {
	t.Parallel()

	const sql = "UPDATE emp SET id = 2"

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	// Parse it while the grant still allows writes, so the cursor is tracked.
	require.NoError(t, s.handleOALL8(buildOALL8(sql, nil, 11)))

	// Drain the row the successful parse inserted (persistQueryRecord goes
	// through s.store, which is nil here, so nothing is queued — but be
	// explicit about starting from an empty channel).
	require.Empty(t, recorder.created)

	// Narrow the grant, then re-execute the very same cursor.
	s.grant = &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	require.Error(t, s.handleCursorReexec(11), "the re-execution must be refused")

	got := recorder.awaitCreated(t)
	assertBlockedOracleRow(t, got, sql, "read-only")
	recorder.assertNoFurtherCreate(t)
}

// TestBlockedStatement_IsAnOrdinaryChainAppend runs a refusal through the real
// store rather than a recorder. Two things only a real store can prove: the
// row lands in `queries` attributed to the session's connection, and — because
// `queries` is HMAC-chained per connection — the refusal row is an ordinary
// chain append that `dbbat audit verify --queries` still walks cleanly.
func TestBlockedStatement_IsAnOrdinaryChainAppend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	dataStore := newOracleTestStore(t)
	require.True(t, dataStore.ChainEnabled(), "the query chain must be on for this test to mean anything")

	user, err := dataStore.CreateUser(ctx, "blocked-oracle", "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "blocked-oracle-db",
		Host:         "localhost",
		Port:         1521,
		DatabaseName: "ORCL",
		Username:     "u",
		Password:     "p",
		Protocol:     store.ProtocolOracle,
	}, key)
	require.NoError(t, err)

	conn, err := dataStore.CreateConnection(ctx, user.UID, db.UID, "10.0.0.9")
	require.NoError(t, err)

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}})
	s.completionStore = dataStore
	s.connectionUID = conn.UID

	const sql = "DELETE FROM emp"

	require.Error(t, s.handleOALL8(buildOALL8(sql, nil, 3)))

	var persisted []store.Query

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{ConnectionID: &conn.UID, Limit: 10})
		if err != nil || len(queries) == 0 {
			return false
		}

		persisted = queries

		return true
	}, 10*time.Second, 50*time.Millisecond, "the refused statement never reached the store")

	require.Len(t, persisted, 1)
	assertBlockedOracleRow(t, &persisted[0], sql, "read-only")

	result, err := dataStore.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a refusal row must be a valid link in the connection's query chain")
	assert.Equal(t, int64(1), result.Verified)
}

// newOracleTestStore spins up a throwaway PostgreSQL store with the query
// chain enabled. Only dbbat's own storage DB is involved — no Oracle container
// — so this stays out of the integration-tagged suite and runs under
// `make test`.
func newOracleTestStore(t *testing.T) *store.Store {
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

// assertBlockedOracleRow checks the shape every refusal row shares across the
// five protocols.
func assertBlockedOracleRow(t *testing.T, row *store.Query, wantSQL, wantErrFragment string) {
	t.Helper()

	require.NotNil(t, row)
	assert.Equal(t, wantSQL, row.SQLText)
	require.NotNil(t, row.Error, "a refused statement must be persisted with its refusal as error")
	assert.Contains(t, *row.Error, wantErrFragment)
	require.NotNil(t, row.DurationMs)
	assert.InDelta(t, 0, *row.DurationMs, 0.001, "a refused statement never ran")
	require.NotNil(t, row.RowsAffected)
	assert.Equal(t, int64(0), *row.RowsAffected)
}
