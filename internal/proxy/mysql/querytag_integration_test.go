//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// tagPrefix is what every tagged statement starts with. The values vary with
// the fixture (version, username, connection uid, grant slug), so the
// assertions check the marker plus the fields that must be present.
const tagPrefix = "/*dbbat='"

// setupTaggedFixture is setupFixture with DBB_QUERY_TAGGING on. The flag is an
// atomic on the server, so turning it on after the listener started is safe —
// and no session exists yet at this point anyway.
func setupTaggedFixture(ctx context.Context, t *testing.T) *fixture {
	t.Helper()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	f.proxy.SetQueryTagging(true)

	return f
}

// pinnedConn takes a single connection out of the pool and keeps it for the
// whole test. It has to be one connection: a dbbat session is one upstream
// MySQL thread, and every assertion below reads performance_schema for *this
// session's* thread. A pooled *sql.DB could hand the lookup a different
// connection, i.e. a different dbbat session and a different upstream thread.
func pinnedConn(ctx context.Context, t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// lastStatement reads back what the *upstream* recorded as this session's most
// recent statement matching like — the bytes dbbat actually sent, as MySQL
// parsed them.
//
// events_statements_history rather than _current, because _current holds the
// statement in flight, which at the moment this runs is this very SELECT.
func lastStatement(ctx context.Context, t *testing.T, conn *sql.Conn, like string) string {
	t.Helper()

	var text string

	require.Eventually(t, func() bool {
		err := conn.QueryRowContext(ctx,
			"SELECT SQL_TEXT FROM performance_schema.events_statements_history "+
				"WHERE THREAD_ID = PS_CURRENT_THREAD_ID() AND SQL_TEXT LIKE ? "+
				"ORDER BY EVENT_ID DESC LIMIT 1", like,
		).Scan(&text)

		return err == nil && text != ""
	}, 10*time.Second, 200*time.Millisecond,
		"performance_schema never showed a statement matching %q", like)

	return text
}

// TestIntegration_QueryTagging_UpstreamSeesTagStoreDoesNot is the spec's
// central assertion: the target receives the tagged statement, and everything
// dbbat persists holds the statement the client sent.
func TestIntegration_QueryTagging_UpstreamSeesTagStoreDoesNot(t *testing.T) {
	ctx := context.Background()

	f := setupTaggedFixture(ctx, t)
	db := f.dialTLS()

	defer db.Close()

	conn := pinnedConn(ctx, t, db)

	const marker = "SELECT 'tagme'"

	var got string
	require.NoError(t, conn.QueryRowContext(ctx, marker).Scan(&got))
	assert.Equal(t, "tagme", got)

	upstreamSaw := lastStatement(ctx, t, conn, "%'tagme'%")

	assert.True(t, strings.HasPrefix(upstreamSaw, tagPrefix),
		"the upstream must have received the tag, got %q", upstreamSaw)
	assert.Contains(t, upstreamSaw, "user='"+fixtureUser+"'")
	assert.True(t, strings.HasSuffix(upstreamSaw, marker),
		"the client's statement must follow the tag verbatim, got %q", upstreamSaw)

	// The conn= value must be this session's own connection uid suffix.
	conns, err := f.store.ListConnections(ctx, store.ConnectionFilter{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, conns)

	hex := strings.ReplaceAll(conns[0].UID.String(), "-", "")
	assert.Contains(t, upstreamSaw, "conn='"+hex[len(hex)-12:]+"'")

	// Nothing dbbat stored may carry it.
	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		for i := range queries {
			if queries[i].SQLText == marker {
				return true
			}
		}

		return false
	}, 10*time.Second, 200*time.Millisecond,
		"the statement was never recorded with the client's exact text")

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
	require.NoError(t, err)

	for i := range queries {
		assert.NotContains(t, queries[i].SQLText, tagPrefix,
			"a stored statement carries the tag — historical query text is now proxy-rewritten")
	}
}

// TestIntegration_QueryTagging_DisabledLeavesBytesUnchanged — with the feature
// off the upstream receives exactly what it received before it existed.
func TestIntegration_QueryTagging_DisabledLeavesBytesUnchanged(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	db := f.dialTLS()

	defer db.Close()

	conn := pinnedConn(ctx, t, db)

	const marker = "SELECT 'untagged'"

	var got string
	require.NoError(t, conn.QueryRowContext(ctx, marker).Scan(&got))
	assert.Equal(t, "untagged", got)

	upstreamSaw := lastStatement(ctx, t, conn, "%'untagged'%")

	assert.Equal(t, marker, upstreamSaw,
		"with tagging off the upstream must see the client's bytes, unchanged")
	assert.NotContains(t, upstreamSaw, "/*dbbat=")
}

// TestIntegration_QueryTagging_PreparedStatementStillWorks — COM_STMT_PREPARE
// carries the tag, COM_STMT_EXECUTE is binary and carries nothing. The
// parameters must still bind and the result must still be right, and the
// statement MySQL holds must be tagged exactly once (a second tag would mean
// something was tagging the execute path too).
func TestIntegration_QueryTagging_PreparedStatementStillWorks(t *testing.T) {
	ctx := context.Background()

	f := setupTaggedFixture(ctx, t)
	db := f.dialTLS()

	defer db.Close()

	conn := pinnedConn(ctx, t, db)

	stmt, err := conn.PrepareContext(ctx, "SELECT ? + ?, ?")
	require.NoError(t, err)

	defer stmt.Close()

	for i := range 3 {
		var (
			sum   int
			label string
		)

		require.NoError(t, stmt.QueryRowContext(ctx, i, 35, "binary").Scan(&sum, &label))
		assert.Equal(t, i+35, sum)
		assert.Equal(t, "binary", label)
	}

	prepared := lastStatement(ctx, t, conn, "%? + ?%")

	assert.True(t, strings.HasPrefix(prepared, tagPrefix),
		"COM_STMT_PREPARE must carry the tag, got %q", prepared)
	assert.Equal(t, 1, strings.Count(prepared, tagPrefix),
		"the tag must be applied once, at prepare — not again per execute")
	assert.True(t, strings.HasSuffix(prepared, "SELECT ? + ?, ?"))
}

// TestIntegration_QueryTagging_ControlsUnchanged — a read_only refusal is the
// same refusal with tagging on as with it off, because the control matched the
// client's text either way.
func TestIntegration_QueryTagging_ControlsUnchanged(t *testing.T) {
	ctx := context.Background()

	refusal := func(tagging bool) string {
		f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
		f.proxy.SetQueryTagging(tagging)
		replaceGrantWithControls(ctx, t, f, store.ControlReadOnly)

		db := f.dialTLS()
		defer db.Close()

		_, err := db.ExecContext(ctx, "CREATE TABLE tagged_refusal (a int)")
		require.Error(t, err, "a read_only grant must refuse a write, tagging or not")

		return err.Error()
	}

	assert.Equal(t, refusal(false), refusal(true),
		"the refusal text must not depend on whether tagging is on")
}

// TestIntegration_QueryTagging_AuditChainVerifies is the spec's own stated
// test. It should be a non-event — the chain never saw a tagged statement — but
// a MAC computed over rewritten text would still verify against itself, so what
// this catches is the chain being built over bytes the client never sent.
func TestIntegration_QueryTagging_AuditChainVerifies(t *testing.T) {
	ctx := context.Background()

	f := setupTaggedFixture(ctx, t)
	db := f.dialTLS()

	for i := range 5 {
		var got int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT 1 + ?", i).Scan(&got))
		assert.Equal(t, 1+i, got)
	}

	require.NoError(t, db.Close())

	require.Eventually(t, func() bool {
		res, err := f.store.VerifyQueryChains(ctx, nil)

		return err == nil && res.Connections > 0 && res.Verified > 0
	}, 20*time.Second, 200*time.Millisecond, "no chained statement was ever written")

	res, err := f.store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	assert.True(t, res.OK(), "query chain break with tagging on: %+v", res.Break)

	auditRes, err := f.store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	assert.True(t, auditRes.OK(), "audit chain break with tagging on: %+v", auditRes.Break)
}
