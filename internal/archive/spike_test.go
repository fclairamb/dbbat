package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
	"github.com/stretchr/testify/require"
)

// writeRows writes n synthetic rows shaped like a real capture (median 31
// bytes of data) and returns the file.
func writeRows(t *testing.T, n int, statements int) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := parquet.NewGenericWriter[RowRecord](&buf, parquet.Compression(&zstd.Codec{}), parquet.MaxRowsPerRowGroup(RowGroupRows))
	conn := uuid.New()
	recs := make([]RowRecord, 0, n)

	for i := range n {
		data, err := json.Marshal(map[string]any{"id": i, "name": fmt.Sprintf("user-%d", i%977), "ok": i%2 == 0})
		require.NoError(t, err)

		recs = append(recs, RowRecord{
			ConnectionUID: conn, QueryUID: uuid.NewSHA1(conn, []byte{byte(i % statements)}), RowUID: uuid.New(),
			RowNumber: int32(i + 1), RowData: string(data), RowSizeBytes: int64(len(data)),
		})
	}

	_, err := w.Write(recs)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return buf.Bytes()
}

// TestSpikeFormat records the phase 4 numbers. It always asserts the round
// trip; the sizes are logged. DuckDB is checked from outside (see the spec).
func TestSpikeFormat(t *testing.T) {
	t.Parallel()

	small := writeRows(t, 3, 2)
	big := writeRows(t, 100_000, 40)

	t.Logf("3 rows, 2 statements: %d bytes", len(small))
	t.Logf("100000 rows, 40 statements: %d bytes (%.1f bytes/row)", len(big), float64(len(big))/100000)

	f, err := parquet.OpenFile(bytes.NewReader(big), int64(len(big)))
	require.NoError(t, err)
	require.Equal(t, int64(100_000), f.NumRows())

	if dir := os.Getenv("DBB_SPIKE_DIR"); dir != "" {
		require.NoError(t, os.WriteFile(dir+"/rows.parquet", big, 0o600))
	}
}
