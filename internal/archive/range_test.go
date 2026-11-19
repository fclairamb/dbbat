package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob" // file:// buckets for the test
)

// TestReadQueryRowsRangeReadsAboveTheWholeObjectLimit writes an object larger
// than WholeObjectLimit (so it is opened over range requests, not fetched
// whole) and reads a statement's rows from the middle of it.
func TestReadQueryRowsRangeReadsAboveTheWholeObjectLimit(t *testing.T) {
	t.Parallel()

	const (
		rowsPerStatement = 6000
		statements       = 2
	)

	conn := uuid.New()
	queries := []uuid.UUID{uuid.New(), uuid.New()}

	var buf bytes.Buffer

	// Uncompressed, random payloads: the object has to be big for real.
	w := parquet.NewGenericWriter[RowRecord](&buf, parquet.MaxRowsPerRowGroup(RowGroupRows))
	offsets := map[string][2]int64{}

	pos := int64(0)

	for qi, q := range queries {
		offsets[q.String()] = [2]int64{pos, rowsPerStatement}

		recs := make([]RowRecord, 0, rowsPerStatement)

		for i := range rowsPerStatement {
			raw := make([]byte, 1024)
			_, err := rand.Read(raw)
			require.NoError(t, err)

			data, err := json.Marshal(map[string]any{"q": qi, "i": i, "blob": base64.StdEncoding.EncodeToString(raw)})
			require.NoError(t, err)

			recs = append(recs, RowRecord{
				ConnectionUID: conn,
				QueryUID:      q,
				RowUID:        uuid.New(),
				RowNumber:     int32(i + 1),
				RowData:       string(data),
				RowSizeBytes:  int64(len(data)),
			})
		}

		_, err := w.Write(recs)
		require.NoError(t, err)

		pos += rowsPerStatement
	}

	offsetsJSON, err := json.Marshal(offsets)
	require.NoError(t, err)

	w.SetKeyValueMetadata(MetaQueryOffsets, string(offsetsJSON))
	require.NoError(t, w.Close())
	require.Greater(t, buf.Len(), WholeObjectLimit, "the fixture must be above the whole-object limit")

	dir := t.TempDir()
	ctx := context.Background()

	bucket, err := blob.OpenBucket(ctx, "file://"+dir)
	require.NoError(t, err)

	t.Cleanup(func() { _ = bucket.Close() })

	require.NoError(t, bucket.WriteAll(ctx, "big.parquet", buf.Bytes(), nil))

	obj := Object{Key: "big.parquet", Size: int64(buf.Len())}

	// A page from the middle of the second statement.
	page, err := ReadQueryRows(ctx, bucket, obj, queries[1], 4000, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(rowsPerStatement), page.Total)
	require.Len(t, page.Rows, 3)

	for i, r := range page.Rows {
		assert.Equal(t, int32(4000+i+1), r.RowNumber)
		assert.Equal(t, queries[1], uuid.UUID(r.QueryUID))
		assert.JSONEq(t, string(mustMarshalPrefix(t, r.RowData)), r.RowData)
	}

	// The first statement's tail, and a page running past the end.
	page, err = ReadQueryRows(ctx, bucket, obj, queries[0], rowsPerStatement-2, 10)
	require.NoError(t, err)
	require.Len(t, page.Rows, 2)
	assert.Equal(t, queries[0], uuid.UUID(page.Rows[0].QueryUID))

	// A missing object is its own error on the range path too.
	_, err = ReadQueryRows(ctx, bucket, Object{Key: "gone.parquet", Size: obj.Size}, queries[0], 0, 1)
	require.ErrorIs(t, err, ErrObjectMissing)
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
