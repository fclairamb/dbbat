package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/store"
)

var (
	containerOnce sync.Once
	containerDSN  string
	errContainer  error
)

// testStore returns a store on a shared PostgreSQL container. Tests in this
// package are sequential and use unique names.
func testStore(t *testing.T) *store.Store {
	t.Helper()

	containerOnce.Do(func() {
		ctx := context.Background()

		c, err := postgres.Run(ctx, "postgres:15-alpine",
			postgres.WithDatabase("dbbat_archive_test"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(90*time.Second)),
		)
		if err != nil {
			errContainer = err

			return
		}

		containerDSN, errContainer = c.ConnectionString(ctx, "sslmode=disable")
	})

	if errContainer != nil {
		t.Skipf("postgres container unavailable: %v", errContainer)
	}

	s, err := store.New(context.Background(), containerDSN)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	return s
}

type fixture struct {
	store  *store.Store
	server *store.Server
	conn   *store.Connection
	// queries holds the uid of each statement and how many rows it captured.
	queries []uuid.UUID
}

// newFixture creates a server (with its own retention windows), a connection
// that ran statements with the given row counts, and closes it `closedAgo`
// ago.
func newFixture(t *testing.T, s *store.Store, name string, archive, del *int64, rowCounts []int, closedAgo time.Duration) fixture {
	t.Helper()

	ctx := context.Background()
	key := make([]byte, 32)

	user, err := s.CreateUser(ctx, "user_"+name, "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	srv, err := s.CreateServer(ctx, &store.Server{
		Name: "srv-" + name, Host: "localhost", Port: 5432, DatabaseName: "db",
		Username: "u", Password: "p", SSLMode: "disable",
	}, key)
	require.NoError(t, err)

	require.NoError(t, s.UpdateServer(ctx, srv.UID, store.ServerUpdate{
		ArchiveAfter: store.OptionalSeconds{Set: archive != nil, Value: archive},
		DeleteAfter:  store.OptionalSeconds{Set: del != nil, Value: del},
	}, key))

	conn, err := s.CreateConnection(ctx, user.UID, srv.UID, "127.0.0.1")
	require.NoError(t, err)

	f := fixture{store: s, server: srv, conn: conn}
	executed := time.Now().Add(-closedAgo - time.Minute)

	for i, n := range rowCounts {
		q, err := s.CreateQuery(ctx, &store.Query{
			ConnectionID: conn.UID, SQLText: "SELECT " + name, ExecutedAt: executed.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)

		f.queries = append(f.queries, q.UID)

		var pending []store.PendingQueryRow

		for r := 1; r <= n; r++ {
			data := map[string]any{"id": r, "q": i, "text": "héllo \"quoted\" ☃", "n": 1.5, "nil": nil}
			raw, err := json.Marshal(data)
			require.NoError(t, err)

			pending = append(pending, store.PendingQueryRow{QueryID: q.UID, RowNumber: r, RowData: raw, RowSizeBytes: int64(len(raw))})
		}

		require.NoError(t, s.StoreQueryRows(ctx, pending))
	}

	require.NoError(t, s.CloseConnectionAt(ctx, conn.UID, time.Now().Add(-closedAgo)))

	return f
}

func openArchiver(t *testing.T, s *store.Store, dir string) *Archiver {
	t.Helper()

	a, err := Open(context.Background(), "file://"+dir, s, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	return a
}

func i64(v int64) *int64 { return &v }

func listFiles(t *testing.T, dir string) []string {
	t.Helper()

	var files []string

	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)

		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}

		return nil
	}))

	return files
}

func TestKeyCharsetAndShape(t *testing.T) {
	t.Parallel()

	srv := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	conn := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	key := RowsKey(time.Date(2026, 11, 17, 23, 59, 59, 0, time.UTC), srv, conn)

	assert.Equal(t, "v1/rows/date=2026-11-17/server=11111111-2222-3333-4444-555555555555/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.parquet", key)
	assert.True(t, ValidKey(key))
	assert.False(t, ValidKey("v1/Rows/a:b"))
	assert.False(t, ValidKey("v1/rows/a b"))

	// The date is the UTC date of disconnected_at, whatever the zone.
	paris := time.FixedZone("x", 2*3600)
	assert.Contains(t, RowsKey(time.Date(2026, 11, 18, 1, 0, 0, 0, paris), srv, conn), "date=2026-11-17")

	// The longest path component is the connection uid and its extension.
	assert.Equal(t, len(conn.String())+len(".parquet"), longestComponent(key))
}

func longestComponent(k string) int {
	longest := 0

	for _, c := range strings.Split(k, "/") {
		longest = max(longest, len(c))
	}

	return longest
}

func TestArchiveRoundTrip(t *testing.T) { //nolint:paralleltest // sequential tests share one database
	s := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Archive an hour after close; never delete.
	f := newFixture(t, s, "roundtrip", i64(3600), nil, []int{3, 0, 2}, 2*time.Hour)
	a := openArchiver(t, s, dir)

	res, err := a.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Archived)

	// Nothing but one .parquet file, under the documented layout.
	files := listFiles(t, dir)
	require.Len(t, files, 1)
	assert.True(t, strings.HasPrefix(files[0], "v1/rows/date="), files[0])
	assert.Contains(t, files[0], "/server="+f.server.UID.String()+"/"+f.conn.UID.String()+".parquet")
	assert.True(t, ValidKey(files[0]))

	// Rows left PostgreSQL, statements stayed.
	for i, q := range f.queries {
		page, err := s.GetQueryRows(ctx, q, "", 100)
		require.NoError(t, err, "statement %d stays in PostgreSQL", i)
		assert.Empty(t, page.Rows)
	}

	ref, err := s.GetConnectionArchive(ctx, f.conn.UID)
	require.NoError(t, err)
	assert.Equal(t, int64(5), ref.Rows)
	assert.Positive(t, ref.Bytes)

	data, err := os.ReadFile(filepath.Join(dir, files[0]))
	require.NoError(t, err)

	sum := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(sum[:]), ref.SHA256, "the sealed hash is the file's hash")
	assert.Equal(t, int64(len(data)), ref.Bytes)

	obj := Object{Key: ref.Key, Size: ref.Bytes, SHA256: ref.SHA256}

	// Each statement's rows come back, in order, bit-exact JSON, with paging.
	page, err := ReadQueryRows(ctx, a.Bucket(), obj, f.queries[0], 0, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(3), page.Total)
	require.Len(t, page.Rows, 3)

	for i, r := range page.Rows {
		assert.Equal(t, int32(i+1), r.RowNumber)
		assert.JSONEq(t, `{"id":`+string(rune('1'+i))+`,"q":0,"text":"héllo \"quoted\" ☃","n":1.5,"nil":null}`, r.RowData)
		assert.Equal(t, f.queries[0], uuid.UUID(r.QueryUID))
		assert.Equal(t, f.conn.UID, uuid.UUID(r.ConnectionUID))
	}

	page, err = ReadQueryRows(ctx, a.Bucket(), obj, f.queries[0], 1, 1)
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, int32(2), page.Rows[0].RowNumber)

	page, err = ReadQueryRows(ctx, a.Bucket(), obj, f.queries[1], 0, 100)
	require.NoError(t, err)
	assert.Empty(t, page.Rows, "a statement with no captured rows")

	page, err = ReadQueryRows(ctx, a.Bucket(), obj, f.queries[2], 0, 100)
	require.NoError(t, err)
	require.Len(t, page.Rows, 2)
	assert.JSONEq(t, `{"id":1,"q":2,"text":"héllo \"quoted\" ☃","n":1.5,"nil":null}`, page.Rows[0].RowData)

	// The generic primitive: rows [offset, offset+limit) of the object.
	recs, err := Read[RowRecord](ctx, a.Bucket(), obj, 3, 10)
	require.NoError(t, err)
	assert.Len(t, recs, 2)

	// A second pass has nothing left to do.
	res, err = a.Pass(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Archived)

	// A tampered object is detected without trusting the bucket.
	tampered := append([]byte{}, data...)
	tampered[len(tampered)/2] ^= 0xff
	require.NoError(t, os.WriteFile(filepath.Join(dir, files[0]), tampered, 0o600))

	_, err = ReadQueryRows(ctx, a.Bucket(), obj, f.queries[0], 0, 10)
	require.ErrorIs(t, err, ErrHashMismatch)

	// A missing object is its own error, never an empty list.
	require.NoError(t, os.Remove(filepath.Join(dir, files[0])))

	_, err = ReadQueryRows(ctx, a.Bucket(), obj, f.queries[0], 0, 10)
	require.ErrorIs(t, err, ErrObjectMissing)
}

// Nothing is archived while the window has not elapsed, and without a window.
func TestArchiveRespectsWindows(t *testing.T) { //nolint:paralleltest // sequential tests share one database
	s := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	notDue := newFixture(t, s, "notdue", i64(7200), nil, []int{2}, 10*time.Minute)
	noWindow := newFixture(t, s, "nowindow", nil, nil, []int{2}, 100*time.Hour)

	a := openArchiver(t, s, dir)

	res, err := a.Pass(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Archived)
	assert.Empty(t, listFiles(t, dir))

	for _, f := range []fixture{notDue, noWindow} {
		page, err := s.GetQueryRows(ctx, f.queries[0], "", 10)
		require.NoError(t, err)
		assert.Len(t, page.Rows, 2, "rows stay in PostgreSQL")
	}
}

// Archive deletion removes the object, the empty directories, and leaves a
// chained entry; the ledger row stays.
func TestArchiveDeletion(t *testing.T) { //nolint:paralleltest // sequential tests share one database
	s := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Archive after 1s, delete after 2h, session closed 3h ago: both are due.
	f := newFixture(t, s, "deletion", i64(1), i64(7200), []int{2}, 3*time.Hour)
	a := openArchiver(t, s, dir)

	res, err := a.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Archived)
	assert.Equal(t, 1, res.Deleted)

	assert.Empty(t, listFiles(t, dir), "the object is gone")

	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		assert.False(t, d.IsDir() && (strings.HasPrefix(d.Name(), "date=") || strings.HasPrefix(d.Name(), "server=")),
			"no empty date= or server= directory is left behind: %s", p)

		return nil
	}))

	_, err = s.GetConnectionArchive(ctx, f.conn.UID)
	require.ErrorIs(t, err, store.ErrArchiveNotFound)

	conn, err := s.GetConnectionByUID(ctx, f.conn.UID)
	require.NoError(t, err)
	assert.NotNil(t, conn.ArchivedAt)
	assert.NotNil(t, conn.ArchiveDeletedAt)

	for _, typ := range []string{store.AuditEventConnectionArchived, store.AuditEventConnectionArchiveDeleted} {
		et := typ
		events, err := s.ListAuditEvents(ctx, store.AuditFilter{EventType: &et})
		require.NoError(t, err)
		assert.NotEmpty(t, events, typ)

		// Excluded from an unfiltered listing, like connection.opened.
		all, err := s.ListAuditEvents(ctx, store.AuditFilter{})
		require.NoError(t, err)

		for _, e := range all {
			assert.NotEqual(t, typ, e.EventType)
		}
	}
}

// A failing bucket never turns into deletion: the archive fails, the rows stay,
// and the statements are not deleted past their delete window either.
func TestArchiveFailureKeepsRows(t *testing.T) { //nolint:paralleltest // sequential tests share one database
	s := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	f := newFixture(t, s, "failing", i64(60), i64(3600), []int{2}, 5*time.Hour)
	a := openArchiver(t, s, dir)

	// The bucket directory disappears: every write fails.
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, []byte("not a directory"), 0o600))

	res, err := a.Pass(ctx)
	require.Error(t, err)
	assert.Zero(t, res.Archived)

	page, err := s.GetQueryRows(ctx, f.queries[0], "", 10)
	require.NoError(t, err)
	assert.Len(t, page.Rows, 2, "rows stay in PostgreSQL")

	instance, err := s.InstanceRetention(ctx)
	require.NoError(t, err)

	_, err = s.CleanupRetention(ctx, store.SweepPolicy{Instance: instance, ArchiveEnabled: true})
	require.NoError(t, err)

	_, err = s.GetQuery(ctx, f.queries[0])
	require.NoError(t, err, "past the delete window, but not archived: the statement is kept")

	page, err = s.GetQueryRows(ctx, f.queries[0], "", 10)
	require.NoError(t, err)
	assert.Len(t, page.Rows, 2)
}

// An archived connection's row chains are not walked and not a break.
func TestVerifyRowChainsCountsArchived(t *testing.T) { //nolint:paralleltest // sequential tests share one database
	s := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	f := newFixture(t, s, "verify", i64(1), nil, []int{2}, time.Hour)
	a := openArchiver(t, s, dir)

	_, err := a.Pass(ctx)
	require.NoError(t, err)

	// The chain is only on when the store has the key; without it the walk is
	// unavailable rather than wrong.
	if _, err := s.VerifyRowChains(ctx, &f.conn.UID); err != nil {
		require.ErrorIs(t, err, store.ErrChainKeyUnavailable)
	}
}

// An archive is deleted against the policy as it is when the object goes, not
// as it was when the pass listed it: a window lengthened (or set to never)
// between the listing and the delete protects the object.
func TestArchiveDeletionRechecksPolicy(t *testing.T) { //nolint:paralleltest // sequential tests share one database
	s := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	key := make([]byte, 32)

	// Archived, never deleted.
	f := newFixture(t, s, "recheck", i64(1), nil, []int{2}, 3*time.Hour)
	a := openArchiver(t, s, dir)

	res, err := a.Pass(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.Archived)

	ref, err := s.GetConnectionArchive(ctx, f.conn.UID)
	require.NoError(t, err)

	cref := store.ConnectionArchiveRef{ConnectionUID: f.conn.UID, Key: ref.Key}

	due, err := s.ArchiveStillDue(ctx, cref, time.Now())
	require.NoError(t, err)
	assert.False(t, due, "no delete window: never due")

	// The operator sets a 2h window: due.
	require.NoError(t, s.UpdateServer(ctx, f.server.UID, store.ServerUpdate{
		DeleteAfter: store.OptionalSeconds{Set: true, Value: i64(7200)},
	}, key))

	due, err = s.ArchiveStillDue(ctx, cref, time.Now())
	require.NoError(t, err)
	assert.True(t, due)

	// ...and then lengthens it to a week before the object is deleted.
	require.NoError(t, s.UpdateServer(ctx, f.server.UID, store.ServerUpdate{
		DeleteAfter: store.OptionalSeconds{Set: true, Value: i64(7 * 24 * 3600)},
	}, key))

	due, err = s.ArchiveStillDue(ctx, cref, time.Now())
	require.NoError(t, err)
	assert.False(t, due, "the lengthened window protects the archive")
	assert.Len(t, listFiles(t, dir), 1)
}
