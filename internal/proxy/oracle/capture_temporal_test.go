package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// temporalSQLMarker uniquely identifies temporalQuery in the fixture.
const temporalSQLMarker = "FROM_TZ"

// temporalExpected is the proxy's decoded string for each column of
// temporalQuery: a DATE, a TIMESTAMP, and a TIMESTAMP WITH TIME ZONE (+05:30).
var temporalExpected = []string{
	"2024-03-15 00:00:00",
	"2024-03-15 14:30:45.123456",
	"2024-03-15 14:30:45.123456 +05:30",
}

// TestDumpReplay_Temporal is the end-to-end check that DATE, TIMESTAMP, and
// TIMESTAMP WITH TIME ZONE decode through the row-capture pipeline. The tz value
// previously fell through to a hex string because the offset hour was read from
// the whole byte instead of its low 6 bits (Oracle Free sets bit 0x40 on a fixed
// offset). The fixture is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraTemporal ./internal/proxy/oracle/
func TestDumpReplay_Temporal(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_temporal.dbbat-dump")

	rows := replayCapturedRows(t, td, temporalSQLMarker)

	require.Len(t, rows, 1, "one row of temporal values")
	require.Len(t, rows[0], len(temporalExpected), "all temporal columns captured")

	for i, want := range temporalExpected {
		assert.Equalf(t, want, rows[0][i], "column %d decoded value", i)
	}
}
