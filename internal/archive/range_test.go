package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob" // file:// buckets for the test
)

const (
	bigRowsPerStatement = 6000
	// bigRowsPerGroup cuts the big fixture into 6 row groups of 2000 rows.
	bigRowsPerGroup = 2000
)

// rangeFixture is one Parquet object written like the archiver writes it.
type rangeFixture struct {
	data    []byte
	conn    uuid.UUID
	queries []uuid.UUID
}

// buildObject writes statements × rowsPerStatement records with payloadBytes
// of random (incompressible) data each. rewrite, when set, edits the
// statement-to-offset metadata before it is written.
func buildObject(
	t *testing.T, statements, rowsPerStatement, rowsPerGroup, payloadBytes int, rewrite func(map[string][2]int64),
) rangeFixture {
	t.Helper()

	f := rangeFixture{conn: uuid.New()}
	for range statements {
		f.queries = append(f.queries, uuid.New())
	}

	var buf bytes.Buffer

	w := parquet.NewGenericWriter[RowRecord](&buf, parquet.MaxRowsPerRowGroup(int64(rowsPerGroup)))
	offsets := map[string][2]int64{}

	pos := int64(0)

	for qi, q := range f.queries {
		offsets[q.String()] = [2]int64{pos, int64(rowsPerStatement)}

		recs := make([]RowRecord, 0, rowsPerStatement)

		for i := range rowsPerStatement {
			raw := make([]byte, payloadBytes)
			_, err := rand.Read(raw)
			require.NoError(t, err)

			data, err := json.Marshal(map[string]any{"q": qi, "i": i, "blob": base64.StdEncoding.EncodeToString(raw)})
			require.NoError(t, err)

			recs = append(recs, RowRecord{
				ConnectionUID: f.conn,
				QueryUID:      q,
				RowUID:        uuid.New(),
				RowNumber:     int32(i + 1),
				RowData:       string(data),
				RowSizeBytes:  int64(len(data)),
			})
		}

		_, err := w.Write(recs)
		require.NoError(t, err)

		pos += int64(rowsPerStatement)
	}

	if rewrite != nil {
		rewrite(offsets)
	}

	offsetsJSON, err := json.Marshal(offsets)
	require.NoError(t, err)

	w.SetKeyValueMetadata(MetaQueryOffsets, string(offsetsJSON))
	require.NoError(t, w.Close())

	f.data = buf.Bytes()

	return f
}

// buildBigObject is above WholeObjectLimit, so it is read over range requests.
func buildBigObject(t *testing.T) rangeFixture {
	t.Helper()

	f := buildObject(t, 2, bigRowsPerStatement, bigRowsPerGroup, 1024, nil)
	require.Greater(t, len(f.data), WholeObjectLimit, "the fixture must be above the whole-object limit")

	return f
}

// seal is what the archiver seals for the fixture, computed here
// independently of chunkHasher: whole hash and, when withChunks, the SHA-256
// of every ChunkBytes slice.
func (f rangeFixture) seal(key string, withChunks bool) Object {
	sum := sha256.Sum256(f.data)
	o := Object{Key: key, Size: int64(len(f.data)), SHA256: hex.EncodeToString(sum[:]), ConnectionUID: f.conn}

	if withChunks {
		o.ChunkBytes = ChunkBytes

		for off := 0; off < len(f.data); off += ChunkBytes {
			c := sha256.Sum256(f.data[off:min(off+ChunkBytes, len(f.data))])
			o.ChunkSHA256 = append(o.ChunkSHA256, hex.EncodeToString(c[:]))
		}
	}

	return o
}

func testBucket(t *testing.T) (context.Context, *blob.Bucket) {
	t.Helper()

	ctx := context.Background()

	bucket, err := blob.OpenBucket(ctx, "file://"+t.TempDir())
	require.NoError(t, err)

	t.Cleanup(func() { _ = bucket.Close() })

	return ctx, bucket
}

// alterRowGroup flips a byte in the middle of the row_data column chunk of
// row group rg.
func alterRowGroup(t *testing.T, data []byte, rg int) []byte {
	t.Helper()

	pf, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	for _, col := range pf.Metadata().RowGroups[rg].Columns {
		if len(col.MetaData.PathInSchema) == 1 && col.MetaData.PathInSchema[0] == "row_data" {
			out := append([]byte{}, data...)
			out[col.MetaData.DataPageOffset+col.MetaData.TotalCompressedSize/2] ^= 0xff

			return out
		}
	}

	require.FailNow(t, "no row_data column")

	return nil
}

// TestReadQueryRowsRangeReadsAboveTheWholeObjectLimit writes an object larger
// than WholeObjectLimit (so it is opened over range requests, not fetched
// whole) and reads a statement's rows from the middle of it, every chunk
// checked against the sealed per-chunk digests.
func TestReadQueryRowsRangeReadsAboveTheWholeObjectLimit(t *testing.T) {
	t.Parallel()

	f := buildBigObject(t)
	ctx, bucket := testBucket(t)
	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", f.data, nil))

	obj := f.seal("big.parquet", true)

	// A page from the middle of the second statement.
	page, err := ReadQueryRows(ctx, bucket, obj, f.queries[1], 4000, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(bigRowsPerStatement), page.Total)
	assert.True(t, page.Verified)
	require.Len(t, page.Rows, 3)

	for i, r := range page.Rows {
		assert.Equal(t, int32(4000+i+1), r.RowNumber)
		assert.Equal(t, f.queries[1], uuid.UUID(r.QueryUID))
		assert.JSONEq(t, string(mustMarshalPrefix(t, r.RowData)), r.RowData)
	}

	// The first statement's tail, and a page running past the end.
	page, err = ReadQueryRows(ctx, bucket, obj, f.queries[0], bigRowsPerStatement-2, 10)
	require.NoError(t, err)
	require.Len(t, page.Rows, 2)
	assert.Equal(t, f.queries[0], uuid.UUID(page.Rows[0].QueryUID))

	// A missing object is its own error on the range path too.
	gone := obj
	gone.Key = "gone.parquet"
	_, err = ReadQueryRows(ctx, bucket, gone, f.queries[0], 0, 1)
	require.ErrorIs(t, err, ErrObjectMissing)

	// A digest list that does not cover the sealed size is never trusted.
	short := obj
	short.ChunkSHA256 = obj.ChunkSHA256[1:]
	_, err = ReadQueryRows(ctx, bucket, short, f.queries[0], 0, 1)
	require.ErrorIs(t, err, ErrHashMismatch)
}

// One row group of a large object rewritten after sealing: the paging read
// that touches it fails, a read of an untouched row group still works.
func TestReadQueryRowsRangeDetectsAnAlteredRowGroup(t *testing.T) {
	t.Parallel()

	f := buildBigObject(t)
	ctx, bucket := testBucket(t)

	obj := f.seal("big.parquet", true)

	// Row group 3 holds the rows [6000, 8000): statement 1, offsets [0, 2000).
	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", alterRowGroup(t, f.data, 3), nil))

	_, err := ReadQueryRows(ctx, bucket, obj, f.queries[1], 1000, 3)
	require.ErrorIs(t, err, ErrHashMismatch)

	page, err := ReadQueryRows(ctx, bucket, obj, f.queries[0], 10, 3)
	require.NoError(t, err)
	require.Len(t, page.Rows, 3)
	assert.True(t, page.Verified)

	// A truncated object fails the same way.
	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", f.data[:len(f.data)-ChunkBytes/2], nil))

	_, err = ReadQueryRows(ctx, bucket, obj, f.queries[1], 1000, 3)
	require.ErrorIs(t, err, ErrHashMismatch)
}

// Offset metadata that points one statement's range at another statement's
// records, or records of another connection, is ErrBadMetadata, on the whole
// path and on the range path.
func TestReadQueryRowsRejectsRecordsOfAnotherStatement(t *testing.T) {
	t.Parallel()

	ctx, bucket := testBucket(t)

	pointAtOther := func(offsets map[string][2]int64) {
		var keys []string
		for k := range offsets {
			keys = append(keys, k)
		}

		offsets[keys[0]], offsets[keys[1]] = offsets[keys[1]], offsets[keys[0]]
	}

	small := buildObject(t, 2, 5, RowGroupRows, 16, pointAtOther)
	require.NoError(t, bucket.WriteAll(ctx, "small.parquet", small.data, nil))

	_, err := ReadQueryRows(ctx, bucket, small.seal("small.parquet", false), small.queries[0], 0, 10)
	require.ErrorIs(t, err, ErrBadMetadata)

	big := buildObject(t, 2, bigRowsPerStatement, bigRowsPerGroup, 1024, pointAtOther)
	require.Greater(t, len(big.data), WholeObjectLimit)
	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", big.data, nil))

	_, err = ReadQueryRows(ctx, bucket, big.seal("big.parquet", true), big.queries[1], 4000, 3)
	require.ErrorIs(t, err, ErrBadMetadata)

	// A range past the end of the file.
	past := buildObject(t, 1, 5, RowGroupRows, 16, func(offsets map[string][2]int64) {
		for k := range offsets {
			offsets[k] = [2]int64{3, 5}
		}
	})
	require.NoError(t, bucket.WriteAll(ctx, "past.parquet", past.data, nil))

	_, err = ReadQueryRows(ctx, bucket, past.seal("past.parquet", false), past.queries[0], 0, 10)
	require.ErrorIs(t, err, ErrBadMetadata)

	// Untouched metadata but the records belong to another connection.
	clean := buildObject(t, 1, 5, RowGroupRows, 16, nil)
	require.NoError(t, bucket.WriteAll(ctx, "clean.parquet", clean.data, nil))

	other := clean.seal("clean.parquet", false)
	other.ConnectionUID = uuid.New()
	_, err = ReadQueryRows(ctx, bucket, other, clean.queries[0], 0, 10)
	require.ErrorIs(t, err, ErrBadMetadata)

	// No connection to check against is never read unchecked.
	none := clean.seal("clean.parquet", false)
	none.ConnectionUID = uuid.Nil
	_, err = ReadQueryRows(ctx, bucket, none, clean.queries[0], 0, 10)
	require.ErrorIs(t, err, ErrBadMetadata)

	// The positive control: the small whole-fetch path is unchanged.
	page, err := ReadQueryRows(ctx, bucket, clean.seal("clean.parquet", false), clean.queries[0], 1, 2)
	require.NoError(t, err)
	assert.True(t, page.Verified)
	assert.Equal(t, int64(5), page.Total)
	require.Len(t, page.Rows, 2)
	assert.Equal(t, int32(2), page.Rows[0].RowNumber)
}

// A large object sealed before chunk digests existed is fetched whole and
// checked up to the legacy ceiling; above it, its rows are served as
// unverified, never as verified.
func TestReadQueryRowsLegacyObjectWithoutChunkDigests(t *testing.T) {
	t.Parallel()

	f := buildBigObject(t)
	ctx, bucket := testBucket(t)
	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", f.data, nil))

	legacy := f.seal("big.parquet", false)

	page, err := ReadQueryRows(ctx, bucket, legacy, f.queries[1], 4000, 3)
	require.NoError(t, err)
	assert.True(t, page.Verified, "under the legacy ceiling the whole object is checked")
	require.Len(t, page.Rows, 3)

	// Above the ceiling (lowered here): read unchecked, and said so.
	page, err = readQueryRows(ctx, bucket, legacy, f.queries[1], 4000, 3, WholeObjectLimit)
	require.NoError(t, err)
	assert.False(t, page.Verified)
	require.Len(t, page.Rows, 3)

	// Altered: the whole fetch catches it.
	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", alterRowGroup(t, f.data, 3), nil))

	_, err = ReadQueryRows(ctx, bucket, legacy, f.queries[1], 1000, 3)
	require.ErrorIs(t, err, ErrHashMismatch)
}

// chunkHasher cuts a stream into ChunkBytes chunks whatever the write sizes.
func TestChunkHasherMatchesSlicedDigests(t *testing.T) {
	t.Parallel()

	data := make([]byte, 3*ChunkBytes+12345)
	_, err := rand.Read(data)
	require.NoError(t, err)

	h := newChunkHasher(ChunkBytes)
	for off, step := 0, 1; off < len(data); off, step = off+step, step*3+7 {
		h.Write(data[off:min(off+step, len(data))])
	}

	want := rangeFixture{data: data}.seal("x", true).ChunkSHA256
	assert.Equal(t, want, h.Sums())
	assert.Len(t, want, int(ChunkCount(int64(len(data)), ChunkBytes)))

	exact := newChunkHasher(4)
	exact.Write([]byte("abcdefgh"))
	assert.Len(t, exact.Sums(), 2, "no empty trailing chunk")
}

// mustMarshalPrefix returns its argument: the JSON round trip is what is checked.
func mustMarshalPrefix(t *testing.T, s string) []byte {
	t.Helper()

	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &v))

	out, err := json.Marshal(v)
	require.NoError(t, err)

	return out
}
