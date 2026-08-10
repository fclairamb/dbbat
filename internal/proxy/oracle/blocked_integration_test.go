//go:build integration

package oracle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestIntegration_BlockedStatementsAreLogged is the Oracle half of spec
// 2026-08-09-log-blocked-statements-pg-oracle, asserted the way the PostgreSQL
// suite asserts it (TestIntegration_ReadOnlyGrant_BlocksWrite /
// TestIntegration_BlockDDL_BlocksCreateTable): a statement dbbat refuses must
// leave a `queries` row behind carrying the refusal as `error`, and the session
// must survive the refusal.
//
// The unit tests in blocked_persist_test.go drive handleOALL8 directly; this
// one drives a real go-ora client through the real proxy against a real Oracle,
// which is the only place the *wire* behaviour is covered — that the ORA error
// dbbat synthesises reaches the client as an error rather than desynchronising
// the session, and that exactly one row is written even though the driver
// pipelines more than one TTC frame per statement.
//
// Both controls share one fixture on purpose: every Oracle container start
// costs minutes, and narrowing the grant between the two halves (replaceGrant +
// a fresh connection, because a session resolves its grant once at auth) is
// exactly what the PostgreSQL fixture does.
func TestIntegration_BlockedStatementsAreLogged(t *testing.T) {
	ctx := context.Background()

	env := startOracleThroughProxy(t, nil)

	// Seed under the permissive grant the fixture starts with.
	_, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_blocked_probe")

	_, err := env.db.ExecContext(ctx, "CREATE TABLE dbbat_blocked_probe (id NUMBER)")
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	t.Cleanup(func() { _, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_blocked_probe") })

	t.Run("read_only refuses a write", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlReadOnly})

		client := env.newClient(t)

		// Reads still work.
		var n int
		require.NoError(t, client.QueryRowContext(ctx, "SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))
		assert.Equal(t, 0, n)

		const writeSQL = "INSERT INTO dbbat_blocked_probe VALUES (1)"

		_, err := client.ExecContext(ctx, writeSQL)
		require.Error(t, err, "an INSERT must be refused under a read_only grant")

		// The session survives its refusal: the proxy owes the client an ORA
		// error, not a dead connection.
		require.NoError(t, client.QueryRowContext(ctx, "SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))
		assert.Zero(t, n, "the refused INSERT must never have reached upstream")

		env.assertBlockedQueryLogged(t, writeSQL, "read-only")
	})

	t.Run("block_ddl refuses a CREATE TABLE", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlBlockDDL})

		client := env.newClient(t)

		// DML still goes through under block_ddl.
		_, err := client.ExecContext(ctx, "INSERT INTO dbbat_blocked_probe VALUES (7)")
		require.NoError(t, err, "DML must still be allowed under a block_ddl grant")

		const ddlSQL = "CREATE TABLE dbbat_blocked_ddl (id NUMBER)"

		_, err = client.ExecContext(ctx, ddlSQL)
		require.Error(t, err, "a CREATE TABLE must be refused under a block_ddl grant")

		// The table was never created, and the session still works.
		var n int
		require.NoError(t, client.QueryRowContext(ctx, "SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))
		assert.Equal(t, 1, n, "the session must survive a refused statement")

		var exists int

		require.NoError(t,
			client.QueryRowContext(ctx,
				"SELECT count(*) FROM user_tables WHERE table_name = 'DBBAT_BLOCKED_DDL'").Scan(&exists))
		assert.Zero(t, exists, "the refused CREATE TABLE must never have reached upstream")

		env.assertBlockedQueryLogged(t, ddlSQL, "DDL")
	})
}

// assertBlockedQueryLogged is the actual point of the test above: the refused
// statement is in `queries`, exactly once, with the refusal as `error`, with
// duration and rows_affected at zero because it never ran, attributed to the
// fixture user's connection — and it is an ordinary link in that connection's
// HMAC chain, so `dbbat audit verify --queries` still walks it cleanly.
func (e *oracleThroughProxy) assertBlockedQueryLogged(t *testing.T, wantSQL, wantErrFragment string) {
	t.Helper()

	ctx := context.Background()

	var matches []store.Query

	require.Eventually(t, func() bool {
		queries, err := e.store.ListQueries(ctx, store.QueryFilter{Limit: 500})
		if err != nil {
			return false
		}

		matches = nil

		for i := range queries {
			if queries[i].SQLText == wantSQL {
				matches = append(matches, queries[i])
			}
		}

		return len(matches) > 0
	}, 30*time.Second, 250*time.Millisecond, "the refused statement %q was never logged", wantSQL)

	require.Len(t, matches, 1, "a refused statement must be logged exactly once")

	row := matches[0]
	assertBlockedOracleRow(t, &row, wantSQL, wantErrFragment)

	conn, err := e.store.GetConnectionByUID(ctx, row.ConnectionID)
	require.NoError(t, err)
	assert.Equal(t, e.user.UID, conn.UserID, "a refusal row joins to its user like any other query")

	result, err := e.store.VerifyQueryChain(ctx, row.ConnectionID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a refusal row must be a valid link in the connection's query chain")
	assert.False(t, result.TruncatedPrefix, "nothing was retained away; the chain starts at seq 1")
	assert.Positive(t, result.Verified, "the chain walk must actually cover rows")
}
