package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

func secs(v int64) *int64 { return &v }

func setServerRetention(t *testing.T, s *Store, uid uuid.UUID, archive, del OptionalSeconds) {
	t.Helper()

	require.NoError(t, s.UpdateServer(context.Background(), uid,
		ServerUpdate{ArchiveAfter: archive, DeleteAfter: del}, testEncryptionKey()))
}

func set(v *int64) OptionalSeconds { return OptionalSeconds{Set: true, Value: v} }

func newTestGroup(t *testing.T, s *Store, name string, members ...uuid.UUID) *ServerGroup {
	t.Helper()

	g, err := s.CreateServerGroup(context.Background(), &ServerGroup{Name: name})
	require.NoError(t, err)
	require.NoError(t, s.SetServerGroupMembers(context.Background(), g.UID, members))

	return g
}

func TestPickWindow(t *testing.T) {
	t.Parallel()

	// delete: the longest wins, never (0) beats everything.
	assert.Equal(t, int64(0), pickWindow(0, 100, true))
	assert.Equal(t, int64(0), pickWindow(100, 0, true))
	assert.Equal(t, int64(200), pickWindow(100, 200, true))
	// archive: the smallest wins, never (0) is the largest.
	assert.Equal(t, int64(100), pickWindow(0, 100, false))
	assert.Equal(t, int64(100), pickWindow(100, 0, false))
	assert.Equal(t, int64(100), pickWindow(100, 200, false))
	assert.Equal(t, int64(0), pickWindow(0, 0, false))
}

func TestParseRetentionParam(t *testing.T) {
	t.Parallel()

	d, err := ParseRetentionParam("90s")
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)

	d, err = ParseRetentionParam("0")
	require.NoError(t, err)
	assert.Zero(t, d)

	for _, bad := range []string{"30d", "-1h", "1500ms", ""} {
		_, err := ParseRetentionParam(bad)
		require.ErrorIs(t, err, ErrRetentionInvalid, bad)
	}
}

func TestResolveRetentionLevels(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	_, srv := createTestUserAndDatabase(t, ctx, s, "resolve")
	instance := ResolveInstanceRetention(RetentionParams{DeleteAfter: "10000s"}, config.RetentionWindows{})

	// Unset at every level: never.
	none, err := s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, ResolveInstanceRetention(RetentionParams{}, config.RetentionWindows{}))
	require.NoError(t, err)
	assert.Zero(t, none[srv.UID].Delete.Duration)
	assert.Zero(t, none[srv.UID].Archive.Duration)
	assert.Equal(t, RetentionLevelNone, none[srv.UID].Delete.Source)

	// Instance only.
	got, err := s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, instance)
	require.NoError(t, err)
	assert.Equal(t, 10000*time.Second, got[srv.UID].Delete.Duration)
	assert.Equal(t, RetentionLevelInstance, got[srv.UID].Delete.Source)
	assert.Equal(t, RetentionSourceParameter, got[srv.UID].Delete.InstanceSource)

	// Two groups: the longest delete window wins, a never (0) beats a number.
	g1 := newTestGroup(t, s, "retention-g1", srv.UID)
	g2 := newTestGroup(t, s, "retention-g2", srv.UID)
	require.NoError(t, s.SetServerGroupRetention(ctx, g1.UID, set(secs(100)), set(secs(500))))
	require.NoError(t, s.SetServerGroupRetention(ctx, g2.UID, set(secs(50)), set(secs(900))))

	got, err = s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, instance)
	require.NoError(t, err)
	assert.Equal(t, 900*time.Second, got[srv.UID].Delete.Duration, "longest delete")
	assert.Equal(t, RetentionLevelServerGroup, got[srv.UID].Delete.Source)
	assert.Equal(t, []uuid.UUID{g2.UID}, got[srv.UID].Delete.GroupUIDs)
	assert.Equal(t, 50*time.Second, got[srv.UID].Archive.Duration, "shortest archive")

	require.NoError(t, s.SetServerGroupRetention(ctx, g1.UID, OptionalSeconds{}, set(secs(0))))

	got, err = s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, instance)
	require.NoError(t, err)
	assert.Zero(t, got[srv.UID].Delete.Duration, "never beats every window")
	assert.Equal(t, RetentionLevelServerGroup, got[srv.UID].Delete.Source)

	// The server's own value overrides both, and 0 overrides what is below.
	setServerRetention(t, s, srv.UID, set(secs(10)), set(secs(60)))

	got, err = s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, instance)
	require.NoError(t, err)
	assert.Equal(t, 60*time.Second, got[srv.UID].Delete.Duration)
	assert.Equal(t, 10*time.Second, got[srv.UID].Archive.Duration)
	assert.Equal(t, RetentionLevelServer, got[srv.UID].Delete.Source)

	setServerRetention(t, s, srv.UID, OptionalSeconds{}, set(secs(0)))

	got, err = s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, instance)
	require.NoError(t, err)
	assert.Zero(t, got[srv.UID].Delete.Duration, "0 at the server means never, whatever is below")
	assert.Equal(t, 10*time.Second, got[srv.UID].Archive.Duration, "an unset field is left alone")

	// null inherits again.
	setServerRetention(t, s, srv.UID, set(nil), set(nil))

	got, err = s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, instance)
	require.NoError(t, err)
	assert.Equal(t, RetentionLevelServerGroup, got[srv.UID].Delete.Source)
}

// Archival applies only when 0 < archive < delete or delete is never.
func TestResolveRetentionArchiveNeedsToPrecedeDelete(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	_, srv := createTestUserAndDatabase(t, ctx, s, "resolve_order")
	setServerRetention(t, s, srv.UID, set(secs(500)), set(secs(100)))

	got, err := s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID},
		ResolveInstanceRetention(RetentionParams{}, config.RetentionWindows{}))
	require.NoError(t, err)
	assert.Zero(t, got[srv.UID].Archive.Duration, "archive >= delete: simply deleted")
	assert.Equal(t, 100*time.Second, got[srv.UID].Delete.Duration)
}

// A soft-deleted server keeps the window it had.
func TestResolveRetentionReadsSoftDeletedServers(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	_, srv := createTestUserAndDatabase(t, ctx, s, "resolve_deleted")
	setServerRetention(t, s, srv.UID, OptionalSeconds{}, set(secs(77)))
	require.NoError(t, s.DeleteServer(ctx, srv.UID))

	got, err := s.ResolveRetentionByServers(ctx, []uuid.UUID{srv.UID}, InstanceRetention{})
	require.NoError(t, err)
	assert.Equal(t, 77*time.Second, got[srv.UID].Delete.Duration)
}

// Nothing set anywhere: the policy read says so, and the sweep deletes nothing.
func TestRetentionOffByDefault(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, s, "retention_off")
	createQueryWithRows(t, ctx, s, conn.UID, "SELECT 1", time.Now().Add(-1000*time.Hour))
	require.NoError(t, s.CloseConnectionAt(ctx, conn.UID, time.Now().Add(-1000*time.Hour)))

	anySet, err := s.AnyRetentionPolicySet(ctx)
	require.NoError(t, err)
	assert.False(t, anySet)

	res, err := s.CleanupRetention(ctx, SweepPolicy{})
	require.NoError(t, err)
	assert.Zero(t, res.Queries)
	assert.Zero(t, res.Connections)

	n, err := s.db.NewSelect().Model((*Query)(nil)).Count(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	require.NoError(t, s.SetRetentionParams(ctx, RetentionParams{DeleteAfter: "720h"}))

	anySet, err = s.AnyRetentionPolicySet(ctx)
	require.NoError(t, err)
	assert.True(t, anySet)
}

// Statements are deleted per resolved window: a server with its own short
// window loses its old statements, another server on the instance default
// keeps its own.
func TestCleanupRetentionPerServerWindow(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	short := createTestConnection(t, ctx, s, "cleanup_short")
	long := createTestConnection(t, ctx, s, "cleanup_long")

	old := time.Now().Add(-2 * time.Hour)
	createQueryWithRows(t, ctx, s, short.UID, "SELECT short", old)
	createQueryWithRows(t, ctx, s, long.UID, "SELECT long", old)

	setServerRetention(t, s, short.DatabaseID, OptionalSeconds{}, set(secs(3600)))

	res, err := s.CleanupRetention(ctx, SweepPolicy{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Queries)

	var texts []string
	require.NoError(t, s.db.NewSelect().Model((*Query)(nil)).Column("sql_text").Scan(ctx, &texts))
	assert.Equal(t, []string{"SELECT long"}, texts)
}

// The ledger is never deleted before its statements, and a live archive blocks
// it; statements of a connection due for archival but not archived are kept
// even past their delete window.
func TestCleanupRetentionLedgerAndFailingBucket(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, s, "cleanup_ledger")
	old := time.Now().Add(-10 * time.Hour)
	createQueryWithRows(t, ctx, s, conn.UID, "SELECT 1", old)
	require.NoError(t, s.CloseConnectionAt(ctx, conn.UID, old))

	// Statement window 1h, archive after 1h... archive needs < delete: use 30m.
	setServerRetention(t, s, conn.DatabaseID, set(secs(1800)), set(secs(3600)))

	policy := SweepPolicy{Connections: 2 * time.Hour, ArchiveEnabled: true}

	// Due for archival, not archived: nothing is deleted, whatever the windows.
	res, err := s.CleanupRetention(ctx, policy)
	require.NoError(t, err)
	assert.Zero(t, res.Queries)
	assert.Zero(t, res.Connections)

	// Archival off: the statements go, then the ledger at max(conn, delete).
	policy.ArchiveEnabled = false
	res, err = s.CleanupRetention(ctx, policy)
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Queries)
	assert.Equal(t, int64(1), res.Connections)
}

func TestConnectionClosedHook(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	var got uuid.UUID

	s.SetConnectionClosedHook(func(serverUID uuid.UUID, _ time.Time) { got = serverUID })

	conn := createTestConnection(t, ctx, s, "hook")
	require.NoError(t, s.CloseConnection(ctx, conn.UID))
	assert.Equal(t, conn.DatabaseID, got)
}

func TestOptionalSecondsJSON(t *testing.T) {
	t.Parallel()

	var req struct {
		A OptionalSeconds `json:"a"`
		B OptionalSeconds `json:"b"`
		C OptionalSeconds `json:"c"`
	}

	require.NoError(t, jsonUnmarshal(`{"a": null, "b": 0}`, &req))
	assert.True(t, req.A.Set)
	assert.Nil(t, req.A.Value)
	assert.True(t, req.B.Set)
	assert.Equal(t, int64(0), *req.B.Value)
	assert.False(t, req.C.Set, "absent leaves the value alone")

	require.Error(t, jsonUnmarshal(`{"a": "soon"}`, &req))
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
