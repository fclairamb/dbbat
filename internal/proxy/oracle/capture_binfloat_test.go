package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// binFloatSQLMarker uniquely identifies binFloatQuery in the fixture.
const binFloatSQLMarker = "BINARY_FLOAT"

// binFloatExpected is the proxy's decoded value for each column of binFloatQuery.
var binFloatExpected = []string{"1.5", "2.5", "-1.5"}

// TestDumpReplay_BinFloat is the end-to-end check that BINARY_FLOAT and
// BINARY_DOUBLE decode through the row-capture pipeline now that values are
// decoded by column type (undoing Oracle's sortable byte transform). The fixture
// is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraBinFloat ./internal/proxy/oracle/
func TestDumpReplay_BinFloat(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_binfloat.dbbat-dump")

	rows := replayCapturedRows(t, td, binFloatSQLMarker)

	require.Len(t, rows, 1, "one row")
	require.Len(t, rows[0], len(binFloatExpected), "all float columns captured")

	for i, want := range binFloatExpected {
		assert.Equalf(t, want, rows[0][i], "column %d decoded value", i)
	}
}
