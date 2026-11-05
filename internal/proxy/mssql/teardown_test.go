package mssql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// waitForClosedConnection waits until the session's connection row is closed
// and returns the statements recorded at that moment. Reading them right after
// the close, without polling, is what proves the teardown wrote its row before
// closing the connection rather than in the background.
func (f *authFixture) waitForClosedConnection(t *testing.T) []store.Query {
	t.Helper()

	ctx := context.Background()

	require.Eventually(t, func() bool {
		connections, err := f.store.ListConnections(ctx, store.ConnectionFilter{Limit: 10})

		return err == nil && len(connections) == 1 && connections[0].DisconnectedAt != nil
	}, 15*time.Second, 20*time.Millisecond)

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 10})
	require.NoError(t, err)

	return queries
}

// TestStatementInFlightAtClientDisconnectIsLogged is security scan finding M6:
// a statement forwarded upstream whose client hangs up before the response was
// dropped at teardown, so it never reached the query history or the chain.
func TestStatementInFlightAtClientDisconnectIsLogged(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
	})

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	// The upstream takes the statement and never answers it.
	fixture.fake.mu.Lock()
	fixture.fake.silent = true
	fixture.fake.mu.Unlock()

	const sql = "DELETE FROM employés WHERE id = 1"

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload(sql)))

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		15*time.Second, 20*time.Millisecond, "the statement must have been forwarded upstream")

	require.NoError(t, client.conn.Close())

	queries := fixture.waitForClosedConnection(t)
	require.Len(t, queries, 1, "the in-flight statement must be logged before the session record closes")
	assert.Equal(t, sql, queries[0].SQLText)
	require.NotNil(t, queries[0].Error)
	assert.Equal(t, errClientDisconnectedMidResponse, *queries[0].Error)
	assert.NotNil(t, queries[0].DurationMs)
	assert.Nil(t, queries[0].RowsAffected)
}

// TestCompletedStatementIsNotLoggedTwiceAtTeardown is the positive control: a
// statement whose response completed is recorded once, and the teardown finds
// nothing in flight to complete again.
func TestCompletedStatementIsNotLoggedTwiceAtTeardown(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
	})

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	fixture.fake.mu.Lock()
	fixture.fake.batchResponse = buildDoneToken(doneCount, 1)
	fixture.fake.mu.Unlock()

	const sql = "UPDATE employés SET nom = 'Zoé' WHERE id = 1"

	fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload(sql))

	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})

		return err == nil && len(queries) == 1
	}, 15*time.Second, 20*time.Millisecond)

	require.NoError(t, client.conn.Close())

	queries := fixture.waitForClosedConnection(t)
	require.Len(t, queries, 1, "a completed statement must not be recorded again at teardown")
	assert.Equal(t, sql, queries[0].SQLText)
	assert.Nil(t, queries[0].Error)
	require.NotNil(t, queries[0].RowsAffected)
	assert.Equal(t, int64(1), *queries[0].RowsAffected)
}
