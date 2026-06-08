package oracle

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// colCountSQLMarker uniquely identifies colCountQuery in the fixture.
const colCountSQLMarker = "LEVEL * 10 FROM dual"

// TestDumpReplay_ColCount is the end-to-end check that the proxy captures rows
// for a query whose column names the scanner cannot detect (a single-char alias
// and an unnamed expression). The fix sources the column count from the describe
// header instead of the scanned names. The fixture is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraColCount ./internal/proxy/oracle/
func TestDumpReplay_ColCount(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_colcount.dbbat-dump")

	rows := replayCapturedRows(t, td, colCountSQLMarker)

	require.Len(t, rows, 4, "all rows captured despite undetectable column names")

	for i, row := range rows {
		require.GreaterOrEqualf(t, len(row), 2, "row %d should have 2 columns", i)

		n, err := strconv.Atoi(row[0])
		require.NoErrorf(t, err, "row %d col0 %q not numeric", i, row[0])

		tenN, err := strconv.Atoi(row[1])
		require.NoErrorf(t, err, "row %d col1 %q not numeric", i, row[1])

		assert.Equalf(t, i+1, n, "row %d col0", i)
		assert.Equalf(t, (i+1)*10, tenN, "row %d col1 (n*10)", i)
	}
}
