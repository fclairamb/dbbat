package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// negNumbersSQLMarker uniquely identifies negNumbersQuery in the fixture.
const negNumbersSQLMarker = "AS bignum"

// negNumbersExpected is the proxy's decoded value for each column of
// negNumbersQuery. Before type-aware decoding the negative columns were captured
// as ASCII garbage; now the NUMBER column type drives formatOracleNumber.
var negNumbersExpected = []string{"-42", "-3.14", "100", "-1000000"}

// TestDumpReplay_NegNumbers is the end-to-end check that negative NUMBERs decode
// correctly through the row-capture pipeline now that values are decoded by
// column type. The fixture is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraNegNumbers ./internal/proxy/oracle/
func TestDumpReplay_NegNumbers(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_negnumbers.dbbat-dump")

	rows := replayCapturedRows(t, td, negNumbersSQLMarker)

	require.Len(t, rows, 1, "one row of numbers")
	require.Len(t, rows[0], len(negNumbersExpected), "all number columns captured")

	for i, want := range negNumbersExpected {
		assert.Equalf(t, want, rows[0][i], "column %d decoded value", i)
	}
}
