//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// refusalTimeout bounds a statement dbbat is expected to refuse.
//
// It should not be needed, and the fact that it is is a bug of its own: the
// frame writeTTCError synthesises is a TTC Response (0x08), not the OER (0x04)
// an Oracle server ends a call with, so a client parses it as something else
// and then blocks reading a message that never arrives. Measured here against
// Oracle 23ai Free, go-ora's ExecContext parks in select forever. That is
// tracked in
// specs/todos/2026-08-10-17-oracle-refusal-frame-hangs-the-client.md; when it
// lands this bound comes out and the assertions below become "the client got
// an ORA error" rather than "the call did not complete".
//
// What this test does assert is the half that works and the half the spec
// asked for: the refused statement never reaches upstream, and it leaves a
// `queries` row carrying the refusal as `error`.
const refusalTimeout = 20 * time.Second

// TestIntegration_BlockedStatementsAreLogged is the Oracle half of spec
// 2026-08-09-log-blocked-statements-pg-oracle, asserted the way the PostgreSQL
// suite asserts it (TestIntegration_ReadOnlyGrant_BlocksWrite /
// TestIntegration_BlockDDL_BlocksCreateTable): a statement dbbat refuses must
// leave a `queries` row behind carrying the refusal as `error`.
//
// The unit tests in blocked_persist_test.go drive handleOALL8 directly; this
// is the only place a real go-ora client, the real proxy and a real Oracle are
// in the same picture — which is how the hang above was found at all.
//
// Both controls share one fixture on purpose: every Oracle container start
// costs minutes, and narrowing the grant between the two halves (replaceGrant
// plus a fresh connection, because a session resolves its grant once at auth)
// is exactly what the PostgreSQL fixture does.
func TestIntegration_BlockedStatementsAreLogged(t *testing.T) {
	ctx := context.Background()

	env := startOracleThroughProxy(t, nil)

	// Seed under the permissive grant the fixture starts with.
	_, _ = env.db.ExecContext(ctx, "DROP TABLE dbbat_blocked_probe")

	_, err := env.db.ExecContext(ctx, "CREATE TABLE dbbat_blocked_probe (id NUMBER)")
	require.NoError(t, err, "the seed DDL must be allowed under an unrestricted grant")

	t.Run("read_only refuses a write", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlReadOnly})

		client := env.newClient(t)

		// Reads still work under read_only.
		var n int
		require.NoError(t, client.QueryRowContext(ctx, "SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))
		assert.Equal(t, 0, n)

		const writeSQL = "INSERT INTO dbbat_blocked_probe VALUES (1)"

		env.mustNotComplete(t, client, writeSQL)

		// Nothing of it reached upstream. Asked over a *fresh* connection: the
		// one that issued the refused statement is left mid-call by the bug
		// above and cannot be trusted to answer anything else.
		assert.Zero(t, env.probeRowCount(t), "the refused INSERT must never have reached upstream")

		env.assertBlockedQueryLogged(t, writeSQL, "read-only")
	})

	t.Run("block_ddl refuses a CREATE TABLE", func(t *testing.T) {
		env.replaceGrant(t, []string{store.ControlBlockDDL})

		client := env.newClient(t)

		// DML still goes through under block_ddl.
		_, err := client.ExecContext(ctx, "INSERT INTO dbbat_blocked_probe VALUES (7)")
		require.NoError(t, err, "DML must still be allowed under a block_ddl grant")
		assert.Equal(t, 1, env.probeRowCount(t), "the allowed INSERT must have reached upstream")

		const ddlSQL = "CREATE TABLE dbbat_blocked_ddl (id NUMBER)"

		env.mustNotComplete(t, client, ddlSQL)

		fresh := env.newClient(t)

		var exists int

		require.NoError(t,
			fresh.QueryRowContext(ctx,
				"SELECT count(*) FROM user_tables WHERE table_name = 'DBBAT_BLOCKED_DDL'").Scan(&exists))
		assert.Zero(t, exists, "the refused CREATE TABLE must never have reached upstream")

		env.assertBlockedQueryLogged(t, ddlSQL, "DDL")
	})
}

// mustNotComplete runs a statement dbbat is expected to refuse and requires
// that it does not succeed. See refusalTimeout for why this is not simply
// `require.Error` on an unbounded call.
func (e *oracleThroughProxy) mustNotComplete(t *testing.T, client *sql.DB, query string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), refusalTimeout)
	defer cancel()

	_, err := client.ExecContext(ctx, query)
	require.Error(t, err, "a refused statement must never report success: %q", query)
}

// probeRowCount reads the seeded table over a connection that has not been
// left mid-call, so it reports what actually reached upstream.
func (e *oracleThroughProxy) probeRowCount(t *testing.T) int {
	t.Helper()

	var n int

	require.NoError(t,
		e.newClient(t).QueryRowContext(context.Background(),
			"SELECT count(*) FROM dbbat_blocked_probe").Scan(&n))

	return n
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
