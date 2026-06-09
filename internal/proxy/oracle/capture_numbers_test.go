package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// numbersSQLMarker uniquely identifies numbersQuery in the fixture.
const numbersSQLMarker = "1234567.89"

// numbersExpected is the proxy's decoded string for each column of numbersQuery.
var numbersExpected = []string{"3.14", "1234567.89", "1000000", "0.25"}

// TestDumpReplay_Numbers is the end-to-end check that the proxy decodes
// fractional NUMBERs correctly through the row-capture pipeline. Before the
// decoder fix, decimals lost their point (3.14 was captured as "314"). The
// fixture is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraNumbers ./internal/proxy/oracle/
func TestDumpReplay_Numbers(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_numbers.dbbat-dump")

	rows := replayCapturedRows(t, td, numbersSQLMarker)

	require.Len(t, rows, 1, "one row of numbers")
	require.Len(t, rows[0], len(numbersExpected), "all number columns captured")

	for i, want := range numbersExpected {
		assert.Equalf(t, want, rows[0][i], "column %d decoded value", i)
	}
}
