package oracle

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mixedSQLMarker uniquely identifies mixedQuery in the fixture.
const mixedSQLMarker = "maybe_day"

// mixedRow is the ground truth for one row of mixedQuery. amount/dayNull drive
// the driver-side check; day/grp/amount strings are the proxy's decoded values.
type mixedRow struct {
	id      int
	grp     int
	amount  float64
	dayNull bool
	day     string // "" when NULL; else "YYYY-MM-DD 00:00:00"
}

// mixedExpectedRow returns the ground truth for row n (1-based) of mixedQuery:
// grp 100 for rows 1-3 then 200; amount = n + 0.5; maybe_day NULL on even rows,
// else DATE '2024-01-01' + n.
func mixedExpectedRow(n int) mixedRow {
	grp := 100
	if n > 3 {
		grp = 200
	}

	day := ""
	if n%2 == 1 {
		day = fmt.Sprintf("2024-01-%02d 00:00:00", 1+n)
	}

	return mixedRow{
		id:      n,
		grp:     grp,
		amount:  float64(n) + 0.5,
		dayNull: n%2 == 0,
		day:     day,
	}
}

// TestDumpReplay_Mixed checks the row-capture pipeline on a result that combines
// all the decoded shapes at once: an incrementing id, a compressed-away repeated
// column, a fractional NUMBER, and a DATE that is NULL on alternating rows.
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraMixed ./internal/proxy/oracle/
func TestDumpReplay_Mixed(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_mixed.dbbat-dump")

	rows := replayCapturedRows(t, td, mixedSQLMarker)

	require.Len(t, rows, 6, "all rows captured")

	for i, row := range rows {
		require.GreaterOrEqualf(t, len(row), 4, "row %d columns", i)

		want := mixedExpectedRow(i + 1)

		id, err := strconv.Atoi(row[0])
		require.NoError(t, err)
		assert.Equalf(t, want.id, id, "row %d id", i)

		assert.Equalf(t, strconv.Itoa(want.grp), row[1], "row %d grp (compressed carry-forward)", i)
		assert.Equalf(t, fmt.Sprintf("%d.5", want.id), row[2], "row %d amount (decimal)", i)
		assert.Equalf(t, want.day, row[3], "row %d maybe_day (DATE / NULL)", i)
	}
}
