package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sortedConnection seeds a connection of the user with the given stats.
func sortedConnection(
	t *testing.T, ctx context.Context, s *Store, user, db uuid.UUID,
	connectedAgo time.Duration, openFor *time.Duration, queries int, bytes int64,
) uuid.UUID {
	t.Helper()

	conn, err := s.CreateConnection(ctx, user, db, "10.9.9.9")
	require.NoError(t, err)

	start := time.Now().Add(-connectedAgo)

	var end *time.Time
	if openFor != nil {
		e := start.Add(*openFor)
		end = &e
	}

	_, err = s.db.ExecContext(ctx,
		"UPDATE connections SET connected_at = ?, disconnected_at = ?, queries = ?, bytes_transferred = ? WHERE uid = ?",
		start, end, queries, bytes, conn.UID)
	require.NoError(t, err)

	return conn.UID
}

func connUIDs(t *testing.T, ctx context.Context, s *Store, f ConnectionFilter) []uuid.UUID {
	t.Helper()

	list, err := s.ListConnections(ctx, f)
	require.NoError(t, err)

	out := make([]uuid.UUID, len(list))
	for i := range list {
		out[i] = list[i].UID
	}

	return out
}

func reversed(in []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}

	return out
}

func TestListConnectionsSort(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, db := createTestUserAndDatabase(t, ctx, s, "sortconn")

	d := func(v time.Duration) *time.Duration { return &v }

	// a: 10s closed, b: 1h closed, c: still open since 30m, so ~30m running.
	a := sortedConnection(t, ctx, s, user.UID, db.UID, 3*time.Hour, d(10*time.Second), 5, 100)
	b := sortedConnection(t, ctx, s, user.UID, db.UID, 2*time.Hour, d(time.Hour), 1, 900)
	c := sortedConnection(t, ctx, s, user.UID, db.UID, 30*time.Minute, nil, 3, 500)

	base := ConnectionFilter{UserID: &user.UID}

	cases := []struct {
		sort string
		desc []uuid.UUID
	}{
		{ConnectionSortDuration, []uuid.UUID{b, c, a}},
		{ConnectionSortQueries, []uuid.UUID{a, c, b}},
		{ConnectionSortBytes, []uuid.UUID{b, c, a}},
	}

	for _, tc := range cases {
		f := base
		f.Sort, f.Order = tc.sort, SortDesc
		assert.Equal(t, tc.desc, connUIDs(t, ctx, s, f), "%s desc", tc.sort)

		f.Order = SortAsc
		assert.Equal(t, reversed(tc.desc), connUIDs(t, ctx, s, f), "%s asc", tc.sort)
	}

	// Default sort is the uid order, newest first; asc reverses it.
	f := base
	assert.Equal(t, []uuid.UUID{c, b, a}, connUIDs(t, ctx, s, f))
	f.Sort, f.Order = ConnectionSortConnectedAt, SortAsc
	assert.Equal(t, []uuid.UUID{a, b, c}, connUIDs(t, ctx, s, f))

	// Sort combines with filters without changing the selected rows.
	f = base
	f.Sort, f.Order, f.ActiveOnly = ConnectionSortQueries, SortAsc, true
	assert.Equal(t, []uuid.UUID{c}, connUIDs(t, ctx, s, f))

	// Offset pages a sorted list.
	f = base
	f.Sort, f.Order, f.Limit, f.Offset = ConnectionSortQueries, SortDesc, 1, 1
	assert.Equal(t, []uuid.UUID{c}, connUIDs(t, ctx, s, f))
}

func TestListConnectionsSortTieFallsBackToUIDDesc(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, db := createTestUserAndDatabase(t, ctx, s, "sorttie")

	first := sortedConnection(t, ctx, s, user.UID, db.UID, time.Hour, nil, 7, 1)
	second := sortedConnection(t, ctx, s, user.UID, db.UID, time.Hour, nil, 7, 1)

	for _, order := range []string{SortAsc, SortDesc} {
		got := connUIDs(t, ctx, s, ConnectionFilter{UserID: &user.UID, Sort: ConnectionSortQueries, Order: order})
		assert.Equal(t, []uuid.UUID{second, first}, got, order)
	}
}

func sortedQuery(
	t *testing.T, ctx context.Context, s *Store, conn uuid.UUID, dur *float64, rows *int64,
) uuid.UUID {
	t.Helper()

	q, err := s.CreateQuery(ctx, &Query{
		ConnectionID: conn,
		SQLText:      "SELECT 1",
		ExecutedAt:   time.Now(),
		DurationMs:   dur,
		RowsAffected: rows,
	})
	require.NoError(t, err)

	return q.UID
}

func queryUIDs(t *testing.T, ctx context.Context, s *Store, f QueryFilter) []uuid.UUID {
	t.Helper()

	list, err := s.ListQueries(ctx, f)
	require.NoError(t, err)

	out := make([]uuid.UUID, len(list))
	for i := range list {
		out[i] = list[i].UID
	}

	return out
}

func TestListQueriesSort(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, db := createTestUserAndDatabase(t, ctx, s, "sortqry")

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.9.9.8")
	require.NoError(t, err)

	f64 := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }

	a := sortedQuery(t, ctx, s, conn.UID, f64(5), i64(300))
	b := sortedQuery(t, ctx, s, conn.UID, f64(50), i64(10))
	c := sortedQuery(t, ctx, s, conn.UID, f64(20), i64(20))
	running := sortedQuery(t, ctx, s, conn.UID, nil, nil)

	base := QueryFilter{ConnectionID: &conn.UID}

	f := base
	f.Sort, f.Order = QuerySortDurationMs, SortDesc
	assert.Equal(t, []uuid.UUID{b, c, a, running}, queryUIDs(t, ctx, s, f))
	f.Order = SortAsc
	assert.Equal(t, []uuid.UUID{a, c, b, running}, queryUIDs(t, ctx, s, f), "NULL last when ascending")

	f.Sort, f.Order = QuerySortRowsAffected, SortDesc
	assert.Equal(t, []uuid.UUID{a, c, b, running}, queryUIDs(t, ctx, s, f))
	f.Order = SortAsc
	assert.Equal(t, []uuid.UUID{b, c, a, running}, queryUIDs(t, ctx, s, f))

	// Default order, and asc on it.
	assert.Equal(t, []uuid.UUID{running, c, b, a}, queryUIDs(t, ctx, s, base))
	f = base
	f.Sort, f.Order = QuerySortExecutedAt, SortAsc
	assert.Equal(t, []uuid.UUID{a, b, c, running}, queryUIDs(t, ctx, s, f))

	// Sort combines with filters: only the selection stays the same.
	f = base
	f.Sort, f.Order, f.UserID = QuerySortDurationMs, SortDesc, &user.UID
	assert.Equal(t, []uuid.UUID{b, c, a, running}, queryUIDs(t, ctx, s, f))

	status := ApprovalPending
	_, err = s.db.ExecContext(ctx, "UPDATE queries SET approval_status = ? WHERE uid = ?", status, c)
	require.NoError(t, err)

	f.ApprovalStatus = &status
	assert.Equal(t, []uuid.UUID{c}, queryUIDs(t, ctx, s, f))
}

func TestListQueriesSortTieFallsBackToUIDDesc(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, db := createTestUserAndDatabase(t, ctx, s, "sortqtie")

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.9.9.7")
	require.NoError(t, err)

	d := 3.0
	first := sortedQuery(t, ctx, s, conn.UID, &d, nil)
	second := sortedQuery(t, ctx, s, conn.UID, &d, nil)

	for _, order := range []string{SortAsc, SortDesc} {
		got := queryUIDs(t, ctx, s, QueryFilter{ConnectionID: &conn.UID, Sort: QuerySortDurationMs, Order: order})
		assert.Equal(t, []uuid.UUID{second, first}, got, order)
	}
}

// setConnQueries stamps the sort key of a seeded connection.
func (f *obsFixture) setConnQueries(t *testing.T, uid uuid.UUID, queries int) {
	t.Helper()

	_, err := f.store.db.ExecContext(f.ctx, "UPDATE connections SET queries = ? WHERE uid = ?", queries, uid)
	require.NoError(t, err)
}

// TestListConnectionsSortComposesWithFilters asserts that a sort neither
// widens nor narrows a filter, for the server group, provenance and user
// filters: the filter picks the rows, the sort only orders them.
func TestListConnectionsSortComposesWithFilters(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "sortfilter")

	require.NoError(t, f.store.AddServerToGroup(f.ctx, f.group.UID, f.dbA.UID))
	require.NoError(t, f.store.AddServerToGroup(f.ctx, f.group.UID, f.dbB.UID))

	approvedGrant := f.requestedGrant(t, false)
	autoGrant := f.requestedGrant(t, true)
	directGrant := f.grant(t, f.dbA.UID)

	approved := f.session(t, f.dbA.UID, &approvedGrant.UID)
	auto := f.session(t, f.dbA.UID, &autoGrant.UID)
	direct := f.session(t, f.dbB.UID, &directGrant.UID)
	outside := f.session(t, f.dbC.UID, nil) // not in the group, no grant

	other, _ := createTestUserAndDatabase(t, f.ctx, f.store, "sortfilter_other")
	foreign, err := f.store.CreateConnection(f.ctx, other.UID, f.dbA.UID, "10.0.0.2")
	require.NoError(t, err)

	f.setConnQueries(t, approved.UID, 10)
	f.setConnQueries(t, auto.UID, 30)
	f.setConnQueries(t, direct.UID, 20)
	f.setConnQueries(t, outside.UID, 40)
	f.setConnQueries(t, foreign.UID, 50)

	list := func(filter ConnectionFilter, order string) []uuid.UUID {
		filter.Sort, filter.Order = ConnectionSortQueries, order

		return connUIDs(t, f.ctx, f.store, filter)
	}

	// Server group: the two group servers' sessions of both users, never C.
	group := ConnectionFilter{ServerGroupUID: &f.group.UID}
	assert.Equal(t, []uuid.UUID{foreign.UID, auto.UID, direct.UID, approved.UID}, list(group, SortDesc))
	assert.Equal(t, []uuid.UUID{approved.UID, direct.UID, auto.UID, foreign.UID}, list(group, SortAsc))

	// Provenance: only the approved and direct sessions, ungranted excluded.
	prov := ConnectionFilter{GrantProvenance: []GrantProvenance{GrantProvenanceApproved, GrantProvenanceDirect}}
	assert.Equal(t, []uuid.UUID{direct.UID, approved.UID}, list(prov, SortDesc))
	assert.Equal(t, []uuid.UUID{approved.UID, direct.UID}, list(prov, SortAsc))

	// User: the filter changes the selection even when a sort is applied.
	mine := ConnectionFilter{UserID: &f.user.UID}
	assert.Equal(t, []uuid.UUID{outside.UID, auto.UID, direct.UID, approved.UID}, list(mine, SortDesc))

	theirs := ConnectionFilter{UserID: &other.UID}
	assert.Equal(t, []uuid.UUID{foreign.UID}, list(theirs, SortDesc))
	assert.NotContains(t, list(theirs, SortAsc), approved.UID)

	// Filters AND together under a sort.
	both := ConnectionFilter{UserID: &other.UID, ServerGroupUID: &f.group.UID}
	assert.Equal(t, []uuid.UUID{foreign.UID}, list(both, SortDesc))
}
