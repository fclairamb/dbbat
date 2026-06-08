package oracle

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compressedSQLMarker uniquely identifies compressedRowsQuery in the fixture.
const compressedSQLMarker = "CASE WHEN LEVEL <= 4"

// compressedRow is one ground-truth row. n is implicit (index+1). A NULL OPT is
// represented as "" — the proxy renders a zero-length value as an empty string.
type compressedRow struct {
	grp string
	opt string
}

// compressedRows is the ground truth for compressedRowsQuery.
func compressedRows() []compressedRow {
	out := make([]compressedRow, 0, 8)

	for n := 1; n <= 8; n++ {
		grp := "AAA"
		if n > 4 {
			grp = "BBB"
		}

		opt := ""
		if n%3 != 0 {
			opt = "x" + strconv.Itoa(n)
		}

		out = append(out, compressedRow{grp: grp, opt: opt})
	}

	return out
}

// TestDumpReplay_CompressedRows is the end-to-end check that the proxy's
// row-capture pipeline correctly handles column compression (values carried
// forward from the previous row) and NULL values. The fixture is regenerated
// with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraCompressedRows ./internal/proxy/oracle/
func TestDumpReplay_CompressedRows(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_compressed.dbbat-dump")

	rows := replayCapturedRows(t, td, compressedSQLMarker)
	want := compressedRows()

	require.Len(t, rows, len(want), "every row should be captured")

	for i, row := range rows {
		require.GreaterOrEqualf(t, len(row), 3, "row %d missing columns", i)

		n, err := strconv.Atoi(row[1])
		require.NoErrorf(t, err, "row %d n %q not numeric", i, row[1])

		assert.Equalf(t, want[i].grp, row[0], "row %d grp (carry-forward)", i)
		assert.Equalf(t, i+1, n, "row %d n out of sequence", i)
		assert.Equalf(t, want[i].opt, row[2], "row %d opt (NULL handling)", i)
	}
}
