package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// archiveTestConnection is one connection with a sealed, stamped capture of
// three rows.
func archiveTestConnection(t *testing.T, ctx context.Context, store *Store, suffix string) (*Connection, *Query) {
	t.Helper()

	conn, queries := createChainTestConnection(t, ctx, store, suffix, 1)
	query := &queries[0]

	storeChainedRows(t, ctx, store, query.UID, 1, 2, 3)
	require.NoError(t, store.SealQueryRowChain(ctx, query.UID))

	return conn, query
}

func testArchiveSeal(conn *Connection) ArchiveSeal {
	return ArchiveSeal{
		ConnectionUID: conn.UID,
		ServerUID:     conn.DatabaseID,
		Key:           "v1/rows/date=2026-11-23/server=" + conn.DatabaseID.String() + "/" + conn.UID.String() + ".parquet",
		Bytes:         1234,
		SHA256:        strings.Repeat("ab", 32),
		Rows:          3,
		Statements:    1,
		PolicySeconds: 60,
		PolicySource:  "instance",
	}
}

// archiveNormally runs the archiver's store path: chained entry, then the row.
func archiveNormally(t *testing.T, ctx context.Context, store *Store, conn *Connection) ArchiveSeal {
	t.Helper()

	seal := testArchiveSeal(conn)
	require.NoError(t, store.SealAndCommitArchive(ctx, seal))

	return seal
}

// The positive control: a connection archived through the real path verifies,
// counted as archived and not walked.
func TestRowChainsExcuseASealedArchive(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := archiveTestConnection(t, ctx, store, "arch_ok")
	archiveNormally(t, ctx, store, conn)

	result, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a sealed archive is not a break")
	require.Equal(t, int64(1), result.Archived)
	require.Equal(t, int64(0), result.Captures)

	scoped, err := store.VerifyRowChains(ctx, &conn.UID)
	require.NoError(t, err)
	require.True(t, scoped.OK())
	require.Equal(t, int64(1), scoped.Archived)

	ref, err := store.GetConnectionArchive(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, testArchiveSeal(conn).SHA256, ref.SHA256)
	require.Equal(t, int64(3), ref.Rows)
}

// Setting archived_at by hand, with no key involved, excuses nothing: the
// deleted rows of a stamped capture are a break.
func TestRowChainsDoNotExcuseAnUnsealedArchivedAt(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, query := archiveTestConnection(t, ctx, store, "arch_forged")

	_, err := store.db.ExecContext(ctx,
		`UPDATE connections SET archived_at = now(), archive_key = 'k', archive_rows_sha256 = 'x',
		 archive_rows_bytes = 1, archive_rows_count = 3 WHERE uid = ?`, conn.UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE query_id = ?`, query.UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "an unsealed archived_at must not hide deleted rows")
	require.Equal(t, int64(0), result.Archived)
	require.Equal(t, int64(1), result.Captures)

	_, err = store.GetConnectionArchive(ctx, conn.UID)
	require.ErrorIs(t, err, ErrArchiveSealMismatch, "an unsealed archive is never read")
}

// A connection.archived entry written without the key (no valid MAC) is no
// seal either.
func TestRowChainsDoNotExcuseAForgedArchiveEntry(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, query := archiveTestConnection(t, ctx, store, "arch_forged_entry")
	seal := testArchiveSeal(conn)

	details, err := json.Marshal(seal)
	require.NoError(t, err)

	seq := int64(1_000_000)
	_, err = store.db.NewInsert().Model(&AuditLog{
		UID: uuid.Must(uuid.NewV7()), EventType: AuditEventConnectionArchived, Details: details,
		CreatedAt: time.Now(), ChainSeq: &seq, PrevMAC: make([]byte, 32), MAC: make([]byte, 32),
	}).Exec(ctx)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET archived_at = now(), archive_key = ?, archive_rows_sha256 = ?,
		 archive_rows_bytes = ?, archive_rows_count = ? WHERE uid = ?`,
		seal.Key, seal.SHA256, seal.Bytes, seal.Rows, conn.UID)
	require.NoError(t, err)

	_, err = store.db.ExecContext(ctx, `DELETE FROM query_rows WHERE query_id = ?`, query.UID)
	require.NoError(t, err)

	result, err := store.VerifyRowChains(ctx, &conn.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "an entry the key did not seal excuses nothing")
	require.Equal(t, int64(0), result.Archived)
}

// Rewriting the archive columns after a real archive is a break of its own,
// and the archive is no longer served.
func TestRowChainsDetectAnArchiveRowThatDisagreesWithItsSeal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, set, what string }{
		{"sha256", "archive_rows_sha256 = 'cafe'", "sha256"},
		{"key", "archive_key = 'v1/rows/other.parquet'", "key"},
		{"rows", "archive_rows_count = 99", "row count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := setupTestStore(t)
			ctx := context.Background()

			conn, _ := archiveTestConnection(t, ctx, store, "arch_edit_"+tc.name)
			archiveNormally(t, ctx, store, conn)

			_, err := store.db.ExecContext(ctx, "UPDATE connections SET "+tc.set+" WHERE uid = ?", conn.UID)
			require.NoError(t, err)

			result, err := store.VerifyRowChains(ctx, nil)
			require.NoError(t, err)
			require.NotNil(t, result.Break)
			require.Contains(t, result.Break.Reason, "archive seal does not match the connection row")
			require.Contains(t, result.Break.Reason, tc.what)
			require.NotNil(t, result.Break.ConnectionUID)
			require.Equal(t, conn.UID, *result.Break.ConnectionUID)

			_, err = store.GetConnectionArchive(ctx, conn.UID)
			require.ErrorIs(t, err, ErrArchiveSealMismatch)
		})
	}
}

// A deleted archive is excused only by a sealed deletion.
func TestRowChainsExcuseOnlyASealedArchiveDeletion(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	deleted, _ := archiveTestConnection(t, ctx, store, "arch_deleted")
	seal := archiveNormally(t, ctx, store, deleted)
	require.NoError(t, store.MarkArchiveDeleted(ctx, ConnectionArchiveRef{ConnectionUID: deleted.UID, Key: seal.Key}))

	result, err := store.VerifyRowChains(ctx, nil)
	require.NoError(t, err)
	require.Nil(t, result.Break, "a sealed deletion is not a break")
	require.Equal(t, int64(1), result.ArchiveDeleted)
	require.Equal(t, int64(0), result.Archived)

	// The same state on the row, without the connection.archive_deleted entry.
	forged, _ := archiveTestConnection(t, ctx, store, "arch_deleted_forged")
	archiveNormally(t, ctx, store, forged)

	_, err = store.db.ExecContext(ctx,
		`UPDATE connections SET archive_deleted_at = now(), archive_key = NULL WHERE uid = ?`, forged.UID)
	require.NoError(t, err)

	result, err = store.VerifyRowChains(ctx, &forged.UID)
	require.NoError(t, err)
	require.NotNil(t, result.Break, "an unsealed deletion must not hide the missing rows")
	require.Equal(t, int64(0), result.ArchiveDeleted)
}

// The per-chunk digests a range read checks come from the sealed entry, and an
// entry sealed without them (before they existed) reads back without any.
func TestConnectionArchiveCarriesSealedChunkDigests(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn, _ := archiveTestConnection(t, ctx, store, "arch_chunks")
	seal := testArchiveSeal(conn)
	seal.ChunkBytes = 1 << 20
	seal.ChunkSHA256 = []string{strings.Repeat("cd", 32), strings.Repeat("ef", 32)}
	require.NoError(t, store.SealAndCommitArchive(ctx, seal))

	ref, err := store.GetConnectionArchive(ctx, conn.UID)
	require.NoError(t, err)
	require.Equal(t, seal.ChunkBytes, ref.ChunkBytes)
	require.Equal(t, seal.ChunkSHA256, ref.ChunkSHA256)

	legacy, _ := archiveTestConnection(t, ctx, store, "arch_legacy")
	archiveNormally(t, ctx, store, legacy)

	ref, err = store.GetConnectionArchive(ctx, legacy.UID)
	require.NoError(t, err)
	require.Zero(t, ref.ChunkBytes)
	require.Empty(t, ref.ChunkSHA256)
}
