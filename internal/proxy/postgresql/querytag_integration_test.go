//go:build integration

package postgresql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/store"
)

// tagPrefix is what every tagged statement starts with. The values themselves
// vary with the fixture (version, username, connection uid, grant slug), so the
// assertions below check the marker plus the fields that must be there.
const tagPrefix = "/*dbbat='"

// selfQuery asks the *upstream* what it is currently running for this backend.
// It is the cheapest possible look at the bytes dbbat actually sent: the answer
// is the very statement being asked, as PostgreSQL received it.
const selfQuery = "SELECT query FROM pg_stat_activity WHERE pid = pg_backend_pid()"

// setupTaggedFixture is setupFixture with DBB_QUERY_TAGGING on. The flag is an
// atomic on the server, so turning it on after the listener started is safe —
// and no session exists yet at this point anyway.
func setupTaggedFixture(ctx context.Context, t *testing.T) *fixture {
	t.Helper()

	f := setupFixture(ctx, t)
	f.proxy.SetQueryTagging(true)

	return f
}

// TestIntegration_QueryTagging_UpstreamSeesTagStoreDoesNot is the spec's
// central assertion, and the one a bug here would corrupt silently: the target
// receives the tagged statement, and everything dbbat persists — the queries
// table, and therefore the tamper-evident chain over it — holds the statement
// the client sent.
func TestIntegration_QueryTagging_UpstreamSeesTagStoreDoesNot(t *testing.T) {
	ctx := context.Background()
	f := setupTaggedFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	var upstreamSaw string
	require.NoError(t, conn.QueryRow(ctx, selfQuery).Scan(&upstreamSaw))

	assert.True(t, strings.HasPrefix(upstreamSaw, tagPrefix),
		"the upstream must have received the tag, got %q", upstreamSaw)
	assert.Contains(t, upstreamSaw, "user='"+fixtureUser+"'")
	assert.Contains(t, upstreamSaw, "conn='")
	assert.True(t, strings.HasSuffix(upstreamSaw, selfQuery),
		"the client's statement must follow the tag verbatim, got %q", upstreamSaw)

	// The conn= value has to be this session's own connection uid suffix, or
	// the tag names a connection that is not the one running the statement.
	connections, err := f.store.ListConnections(ctx, store.ConnectionFilter{UserID: &f.user.UID})
	require.NoError(t, err)
	require.NotEmpty(t, connections)

	hex := strings.ReplaceAll(connections[0].UID.String(), "-", "")
	assert.Contains(t, upstreamSaw, "conn='"+hex[len(hex)-12:]+"'")

	// And nothing dbbat stored may carry it.
	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		for i := range queries {
			if queries[i].SQLText == selfQuery {
				return true
			}
		}

		return false
	}, 10*time.Second, 100*time.Millisecond,
		"the statement was never recorded with the client's exact text")

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
	require.NoError(t, err)

	for i := range queries {
		assert.NotContains(t, queries[i].SQLText, tagPrefix,
			"a stored statement carries the tag — historical query text is now proxy-rewritten")
	}
}

// TestIntegration_QueryTagging_DisabledLeavesBytesUnchanged is the other half:
// with the feature off, the target receives exactly what it received before
// this feature existed.
func TestIntegration_QueryTagging_DisabledLeavesBytesUnchanged(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	var upstreamSaw string
	require.NoError(t, conn.QueryRow(ctx, selfQuery).Scan(&upstreamSaw))

	assert.Equal(t, selfQuery, upstreamSaw,
		"with tagging off the upstream must see the client's bytes, unchanged")
	assert.NotContains(t, upstreamSaw, "/*dbbat=")
}

// TestIntegration_QueryTagging_VisibleInPgStatActivity is the DBA's view: a
// long-running statement, observed from another session, is attributable.
func TestIntegration_QueryTagging_VisibleInPgStatActivity(t *testing.T) {
	ctx := context.Background()
	f := setupTaggedFixture(ctx, t)

	sleeper := f.mustConnect(ctx, fixturePass)
	observer := f.mustConnect(ctx, fixturePass)

	sleepCtx, cancelSleep := context.WithCancel(ctx)
	defer cancelSleep()

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = sleeper.Exec(sleepCtx, "SELECT pg_sleep(10)")
	}()

	var seen string

	require.Eventually(t, func() bool {
		rows, err := observer.Query(ctx,
			"SELECT query FROM pg_stat_activity WHERE query LIKE '%pg_sleep(10)%' AND pid <> pg_backend_pid()")
		if err != nil {
			return false
		}
		defer rows.Close()

		for rows.Next() {
			var q string
			if err := rows.Scan(&q); err != nil {
				return false
			}

			if strings.HasPrefix(q, tagPrefix) {
				seen = q

				return true
			}
		}

		return false
	}, 15*time.Second, 200*time.Millisecond,
		"pg_stat_activity never showed a tagged pg_sleep")

	assert.Contains(t, seen, "user='"+fixtureUser+"'")
	assert.True(t, strings.HasSuffix(seen, "SELECT pg_sleep(10)"))

	cancelSleep()
	<-done
}

// TestIntegration_QueryTagging_ExtendedProtocolRoundTrip — pgx uses the
// extended protocol, so this exercises Parse/Bind/Execute end to end: the
// parameters still bind, the result is still right, and the text upstream
// parsed carries the tag exactly once (it is applied at Parse, not per
// Execute).
func TestIntegration_QueryTagging_ExtendedProtocolRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := setupTaggedFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	// A named, cached prepared statement executed several times.
	const stmt = "SELECT $1::int + $2::int"

	for i := range 3 {
		var sum int
		require.NoError(t, conn.QueryRow(ctx, stmt, i, 10).Scan(&sum))
		assert.Equal(t, i+10, sum)
	}

	// The statement PostgreSQL holds for that prepared statement is tagged,
	// once. A second tag would mean Execute was tagging too.
	//
	// The list is scanned in Go rather than filtered in SQL: a LIKE pattern
	// specific enough to find this statement would also match the literal
	// inside the lookup statement itself, which PostgreSQL prepares too.
	rows, err := conn.Query(ctx, "SELECT statement FROM pg_prepared_statements")
	require.NoError(t, err)

	var prepared string

	for rows.Next() {
		var text string
		require.NoError(t, rows.Scan(&text))

		if strings.HasSuffix(text, stmt) {
			prepared = text
		}
	}

	rows.Close()
	require.NoError(t, rows.Err())
	require.NotEmpty(t, prepared, "the prepared statement was not found upstream")

	assert.True(t, strings.HasPrefix(prepared, tagPrefix), "got %q", prepared)
	assert.Equal(t, 1, strings.Count(prepared, tagPrefix),
		"the tag must be applied once, at Parse — not again per Execute")

	// And what dbbat recorded is the client's text.
	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		for i := range queries {
			if queries[i].SQLText == stmt {
				return true
			}
		}

		return false
	}, 10*time.Second, 100*time.Millisecond, "the prepared statement was never recorded untagged")
}

// TestIntegration_QueryTagging_CopyFromStdinStillLoads — COPY is tagged like
// any statement; the CopyData stream that follows it is a different message
// type and is never touched. If it were, the load would fail or corrupt.
func TestIntegration_QueryTagging_CopyFromStdinStillLoads(t *testing.T) {
	ctx := context.Background()
	f := setupTaggedFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	_, err := conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS tagcopy (id int, name text)")
	require.NoError(t, err)

	rows := [][]any{
		{1, "alpha"},
		{2, "bravo"},
		{3, "charlie"},
	}

	copied, err := conn.CopyFrom(ctx,
		pgx.Identifier{"tagcopy"}, []string{"id", "name"}, pgx.CopyFromRows(rows))
	require.NoError(t, err)
	assert.EqualValues(t, 3, copied)

	var (
		count int
		names string
	)

	require.NoError(t, conn.QueryRow(ctx,
		"SELECT count(*), string_agg(name, ',' ORDER BY id) FROM tagcopy").Scan(&count, &names))
	assert.Equal(t, 3, count)
	assert.Equal(t, "alpha,bravo,charlie", names)
}

// TestIntegration_QueryTagging_ControlsUnchanged — a read_only refusal is the
// same refusal with tagging on as with it off, because the control matched the
// client's text either way.
func TestIntegration_QueryTagging_ControlsUnchanged(t *testing.T) {
	ctx := context.Background()

	refusal := func(tagging bool) string {
		f := setupFixture(ctx, t)
		f.proxy.SetQueryTagging(tagging)

		f.replaceGrant(ctx, []string{store.ControlReadOnly})

		conn := f.mustConnect(ctx, fixturePass)

		_, err := conn.Exec(ctx, "CREATE TABLE tagged_refusal (a int)")
		require.Error(t, err, "a read_only grant must refuse a write, tagging or not")

		return err.Error()
	}

	assert.Equal(t, refusal(false), refusal(true),
		"the refusal text must not depend on whether tagging is on")
}

// TestIntegration_QueryTagging_AuditChainVerifies is the spec's own stated
// test. It should be a non-event — the chain never saw a tagged statement — but
// "should be" is exactly what a test is for: a MAC computed over rewritten text
// would still verify against itself, so the failure this catches is the chain
// being built over bytes the client never sent.
func TestIntegration_QueryTagging_AuditChainVerifies(t *testing.T) {
	ctx := context.Background()
	f := setupTaggedFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	for i := range 5 {
		var got int
		require.NoError(t, conn.QueryRow(ctx, "SELECT $1::int", i).Scan(&got))
		assert.Equal(t, i, got)
	}

	require.NoError(t, conn.Close(ctx))

	require.Eventually(t, func() bool {
		res, err := f.store.VerifyQueryChains(ctx, nil)

		return err == nil && res.Connections > 0 && res.Verified > 0
	}, 15*time.Second, 200*time.Millisecond, "no chained statement was ever written")

	res, err := f.store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	assert.True(t, res.OK(), "query chain break with tagging on: %+v", res.Break)

	auditRes, err := f.store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	assert.True(t, auditRes.OK(), "audit chain break with tagging on: %+v", auditRes.Break)
}

// TestIntegration_QueryTagging_ApprovalHoldStoresClientText is the
// integration-level form of the invariant that matters most, and the one the
// unit tests cannot reach: the parked row is written by the **real** store, so
// the text this asserts on is the text that actually went into the
// tamper-evident `queries` chain — not a fake's copy of it.
//
// It checks all three halves of a hold under tagging: the pattern still parks
// the statement (it is anchored at `^`, which a prepended tag would defeat if
// the match ran on the forwarded text), the persisted row carries the client's
// statement untagged, and the statement that finally reaches the upstream after
// approval carries the tag.
func TestIntegration_QueryTagging_ApprovalHoldStoresClientText(t *testing.T) {
	ctx := context.Background()

	f := setupFixtureWith(ctx, t, fixtureOpts{approvalPatterns: []string{`^DELETE\s+FROM`}})
	f.proxy.SetQueryTagging(true)

	require.NotNil(t, f.approvals, "the fixture must have wired an approval registry")

	setup := f.mustConnect(ctx, fixturePass)
	_, err := setup.Exec(ctx, "CREATE TABLE IF NOT EXISTS tagheld (id int)")
	require.NoError(t, err)
	_, err = setup.Exec(ctx, "INSERT INTO tagheld (id) VALUES (1), (2), (3)")
	require.NoError(t, err)

	held := f.mustConnect(ctx, fixturePass)

	const clientSQL = "DELETE FROM tagheld WHERE id = 2"

	outcome := make(chan error, 1)

	go func() {
		_, execErr := held.Exec(ctx, clientSQL)
		outcome <- execErr
	}()

	// (1) The anchored pattern still parks it.
	var pending store.Query

	require.Eventually(t, func() bool {
		queries, qErr := f.store.ListQueries(ctx, store.QueryFilter{Limit: 100})
		if qErr != nil {
			return false
		}

		for i := range queries {
			if queries[i].ApprovalStatus != nil && *queries[i].ApprovalStatus == store.ApprovalPending {
				pending = queries[i]

				return true
			}
		}

		return false
	}, 30*time.Second, 200*time.Millisecond,
		"the statement was never parked — an anchored pattern stopped matching under tagging")

	// (2) The row an approver reads, and the row the chain MACs, is the
	// client's statement.
	assert.Equal(t, clientSQL, pending.SQLText,
		"the parked row must carry the client's statement, untagged")
	assert.NotContains(t, pending.SQLText, tagPrefix)

	if pending.ApprovalPattern != nil {
		assert.Equal(t, `^DELETE\s+FROM`, *pending.ApprovalPattern)
	}

	by := f.user.UID
	f.approvals.Resolve(approval.Decision{
		QueryUID: pending.UID,
		Status:   store.ApprovalApproved,
		By:       &by,
		ByName:   fixtureUser,
	})

	select {
	case execErr := <-outcome:
		require.NoError(t, execErr, "the approved statement must have run")
	case <-time.After(30 * time.Second):
		t.Fatal("the approved statement never resumed")
	}

	// It really ran.
	var remaining int
	require.NoError(t, setup.QueryRow(ctx, "SELECT count(*) FROM tagheld").Scan(&remaining))
	assert.Equal(t, 2, remaining)

	// (3) What the upstream received after the hold resolved carries the tag.
	var upstreamSaw string
	require.NoError(t, setup.QueryRow(ctx,
		"SELECT query FROM pg_stat_activity WHERE query LIKE '%tagheld WHERE id = 2%' "+
			"AND pid <> pg_backend_pid() LIMIT 1").Scan(&upstreamSaw))

	assert.True(t, strings.HasPrefix(upstreamSaw, tagPrefix),
		"the released statement must have reached the upstream tagged, got %q", upstreamSaw)
	assert.True(t, strings.HasSuffix(upstreamSaw, clientSQL))

	// (4) And the settled row is still the client's text.
	settled, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 100})
	require.NoError(t, err)

	for i := range settled {
		assert.NotContains(t, settled[i].SQLText, tagPrefix,
			"a stored statement carries the tag — the chain would be MACing rewritten text")
	}
}
