package oracle

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// largeResultSQLMarker uniquely identifies the captured large-result SELECT in
// the fixture (see TestCapture_GoOraLargeResult).
const largeResultSQLMarker = "RPAD('v'||LEVEL"

// largeResultRows is the row count of the TestCapture_GoOraLargeResult SELECT.
// The replay test asserts the proxy captures exactly this many sequential rows
// from the fixture; the capture test asserts the driver fetched that many.
const largeResultRows = 400

// largeResultPayload reproduces the second SELECT column for row id n:
// RPAD('v'||n, 60, '.'). It is the ground truth for the captured payload value.
func largeResultPayload(n int) string {
	s := "v" + strconv.Itoa(n)
	if len(s) < 60 {
		s += strings.Repeat(".", 60-len(s))
	}

	return s
}

// replayCapturedRows walks a dump and reproduces what the session row-capture
// pipeline would store for the first query whose SQL contains sqlMarker: it
// feeds the QRESULT (func=0x10) and any following CONTINUATION (func=0x06)
// packets through the same decoders as interceptUpstreamMessage, threading
// lastRow across packets exactly like the session does.
//
// It returns every captured row (first column kept as the row id string).
func replayCapturedRows(t *testing.T, td *testDump, sqlMarker string) [][]string {
	t.Helper()

	var (
		started  bool
		done     bool
		columns  []string
		colTypes []int
		lastRow  []string
		rows     [][]string
	)

	for _, pkt := range td.Packets {
		if done {
			break
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		funcCode, err := parseTTCFunctionCode(tns.Payload)
		if err != nil {
			continue
		}

		ttcPayload := extractTTCPayload(tns.Payload)

		if pkt.Direction == dump.DirClientToServer {
			if sql := findSQLInPayload(ttcPayload); strings.Contains(sql, sqlMarker) {
				started = true
			} else if started && sql != "" {
				done = true // a new statement begins → the result set is over
			}

			continue
		}

		if !started {
			continue
		}

		switch funcCode { //nolint:exhaustive // only row-bearing response codes matter here
		case TTCFuncQueryResult:
			result := decodeQueryResultV2(ttcPayload)
			if result == nil {
				continue
			}

			if len(result.Columns) > 0 {
				columns = result.Columns
				colTypes = result.ColumnTypes
			}

			for _, row := range result.Rows {
				rows = append(rows, row)
				lastRow = row
			}
		case TTCFuncContinuation:
			contRows := parseContinuationRows(ttcPayload, len(columns), lastRow, colTypes)
			for _, row := range contRows {
				strRow := make([]string, len(row))
				for i, v := range row {
					if s, ok := v.(string); ok {
						strRow[i] = s
					}
				}

				rows = append(rows, strRow)
				lastRow = strRow
			}
		}
	}

	return rows
}

// TestDumpReplay_LargeResultRows is the end-to-end check that the proxy captures
// every row of a multi-packet result set. The fixture (400 sequential rows in a
// single large QRESULT) is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraLargeResult ./internal/proxy/oracle/
func TestDumpReplay_LargeResultRows(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_largeresult.dbbat-dump")

	rows := replayCapturedRows(t, td, largeResultSQLMarker)

	require.Len(t, rows, largeResultRows, "every row in the result set should be captured")

	for i, row := range rows {
		require.GreaterOrEqualf(t, len(row), 2, "row %d missing columns", i)

		id, err := strconv.Atoi(strings.TrimSpace(row[0]))
		require.NoErrorf(t, err, "row %d id %q not numeric", i, row[0])
		assert.Equalf(t, i+1, id, "row %d id out of sequence", i)
		assert.Equalf(t, largeResultPayload(id), row[1], "row %d payload mismatch", i)
	}
}
