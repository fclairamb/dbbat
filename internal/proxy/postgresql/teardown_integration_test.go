//go:build integration

package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestIntegration_ClientHangupMidResult_CountsAgainstByteQuota is security
// finding M5: a client that reads part of a large result and closes its socket
// used to leave the statement open and its streamed bytes out of the store, so
// the grant's byte counter — recomputed from the store on every connect — never
// saw them. Stream just under the cap, disconnect, reconnect, repeat.
func TestIntegration_ClientHangupMidResult_CountsAgainstByteQuota(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)

	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
	}

	dbUID, err := uuid.Parse(f.dbUID)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, f.store, f.user.UID, dbUID, []string{},
		testsupport.WithMaxBytesTransferred(1<<30))
	require.NoError(t, err)

	conn, err := f.connect(ctx, fixtureUser, fixturePass)
	require.NoError(t, err)

	connUID := liveConnectionUID(ctx, t, f)

	const sql = "SELECT repeat('x', 1000) FROM generate_series(1, 50000)"

	rows, err := conn.Query(ctx, sql)
	require.NoError(t, err)

	const read = 200

	for i := 0; i < read; i++ {
		require.True(t, rows.Next(), "row %d", i)
	}

	// Hang up mid-result: close the socket under the driver, no Terminate.
	require.NoError(t, conn.PgConn().Conn().Close())

	require.Eventually(t, func() bool {
		c, err := f.store.GetConnectionByUID(ctx, connUID)

		return err == nil && c.DisconnectedAt != nil
	}, 15*time.Second, 100*time.Millisecond, "the session never closed its connection row")

	closed, err := f.store.GetConnectionByUID(ctx, connUID)
	require.NoError(t, err)
	assert.Greater(t, closed.BytesTransferred, int64(read*1000),
		"the bytes streamed before the hangup must reach the connection row")

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &connUID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, queries, 1)
	require.NotNil(t, queries[0].DurationMs, "the cut-off statement must be completed")
	require.NotNil(t, queries[0].Error)
	assert.Equal(t, errClientDisconnectedMidResponse, *queries[0].Error)

	// Reconnect: the grant's counter, recomputed from the store, includes them.
	grant, err := f.store.GetActiveGrant(ctx, f.user.UID, dbUID)
	require.NoError(t, err)
	assert.Equal(t, closed.BytesTransferred, grant.BytesTransferred)

	// Positive control: a session whose statements completed before it ended
	// with Terminate adds only its wire bytes, and no error row.
	conn2, err := f.connect(ctx, fixtureUser, fixturePass)
	require.NoError(t, err)

	var one int
	require.NoError(t, conn2.QueryRow(ctx, "SELECT 1").Scan(&one))

	var conn2UID uuid.UUID

	require.Eventually(t, func() bool {
		live, err := f.store.ListConnections(ctx, store.ConnectionFilter{ActiveOnly: true})
		if err != nil || len(live) != 1 {
			return false
		}

		conn2UID = live[0].UID

		return true
	}, 10*time.Second, 100*time.Millisecond)

	require.NoError(t, conn2.Close(ctx))

	require.Eventually(t, func() bool {
		c, err := f.store.GetConnectionByUID(ctx, conn2UID)

		return err == nil && c.DisconnectedAt != nil
	}, 15*time.Second, 100*time.Millisecond)

	clean, err := f.store.GetConnectionByUID(ctx, conn2UID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), clean.Queries, "the trailing flush is bytes only")

	cleanQueries, err := f.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &conn2UID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, cleanQueries, 1)
	assert.Nil(t, cleanQueries[0].Error, "a completed statement gains no teardown error")

	after, err := f.store.GetActiveGrant(ctx, f.user.UID, dbUID)
	require.NoError(t, err)
	assert.Equal(t, closed.BytesTransferred+clean.BytesTransferred, after.BytesTransferred,
		"the grant counts each session's bytes exactly once")
}
