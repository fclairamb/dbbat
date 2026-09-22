package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// The fourth family of column that does not send a length-prefixed datum, and
// the one that had no recording until 2026-09-22.
//
// go-ora's default LOB policy works by re-declaring a CLOB as a LONG VARCHAR
// and a BLOB as a LONG RAW, so the "inlined LOB" shape the LOB reading learned
// was always a LONG column's shape. Whether a column the **describe** itself
// reports as LONG (8) or LONG RAW (24) carried the same trailer was an
// inference from that substitution; these two recordings are the measurement,
// and the answer is yes, on both thin drivers, down to the `81 01` / `02 05 7d`
// a NULL spells its indicator and return code with.
//
// What the inference did not predict is the **value's** encoding: a genuine
// LONG arrives as a 0xFE long-form CLR whatever its length, where an inlined
// LOB arrived short-form. See readInlineLongColumn.

// longRows and longRawRows are what each fetch captures: the ordinary columns
// verbatim, the LONG column's own value, and the NULL row's empty string.
//
// The LONG RAW value is hex because decodeRowValue renders type 24 as hex — the
// column is binary, and a byte run that happens to be printable is not text.
// That is the same rule the RAW column follows and the reason `DEADBEEF` was
// chosen over something ASCII: it cannot be mistaken for either.
var (
	longRows = [][]string{
		{"aaaaaa", "longvalue-0123456789", "bbbbbb", "cccccc"},
		{"aaaaaa", "", "bbbbbb", "cccccc"},
	}

	longRawRows = [][]string{
		{"dddddd", "deadbeef", "eeeeee", "ffffff"},
		{"dddddd", "", "eeeeee", "ffffff"},
	}
)

// TestThinLongFetchCapturesItsValue replays both recordings through the same
// pipeline the session uses, and the finding is in the expectation rather than
// in any assertion about bytes: every row comes back, with the LONG column's
// own value in it.
//
// Before the reading, all four fetches captured **nothing**. The walk read the
// value and then started the next column on the indicator byte, so the columns
// behind it drifted and rowEndsAtMarker refused the whole row — the same silent
// "no rows" the LOB reading exists to end, one type family over.
//
// Both drivers, because the two disagree about LOB framing and nothing said
// they would agree here. They do.
func TestThinLongFetchCapturesItsValue(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{goOraLongFixture, pythonThinLongFixture} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			require.Contains(t, longQuery, longSQLMarker,
				"the marker has to be a substring of the statement on the wire, or it selects nothing")
			require.Contains(t, longRawQuery, longRawSQLMarker)

			td := loadTestDump(t, fixture)

			assert.Equal(t, longRows, replayCapturedRows(t, td, longSQLMarker))
			assert.Equal(t, longRawRows, replayCapturedRows(t, td, longRawSQLMarker))
		})
	}
}

// TestLongFetchDescribesItsColumnsAsLONG is what makes the fixtures worth
// having: the type codes come off the server's own describe, so the columns are
// LONG because Oracle said so, not because a client re-declared them.
func TestLongFetchDescribesItsColumnsAsLONG(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{goOraLongFixture, pythonThinLongFixture} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, fixture)

			assert.Equal(t, describeColumnTypes(longColumns),
				recordedLongFetch(t, td, longSQLMarker).colTypes)
			assert.Equal(t, describeColumnTypes(longRawColumns),
				recordedLongFetch(t, td, longRawSQLMarker).colTypes)
		})
	}
}

// TestLongFetchIsNotOfferedTheScalarReading is the gate every reading in this
// package carries, and here it is the whole of the bug report: a fetch offered
// two readings is a fetch with two chances at a plausible-looking row.
//
// Each recorded fetch is re-read with its LONG column declared an ordinary
// VARCHAR — one type code changed, nothing else — which is exactly the reading
// dbbat had before this measurement. Every one must come back with nothing: the
// walk comes out on the indicator byte and rowEndsAtMarker costs it the row,
// which is why the defect was a silent empty capture rather than a wrong value.
func TestLongFetchIsNotOfferedTheScalarReading(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{goOraLongFixture, pythonThinLongFixture} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, fixture)

			for _, marker := range []string{longSQLMarker, longRawSQLMarker} {
				fetch := recordedLongFetch(t, td, marker)

				require.NotEmpty(t, fetch.rowsUnder(fetch.colTypes),
					"the fetch must yield its rows under the reading it was recorded from")

				assert.Empty(t, fetch.rowsUnder(scalarizeLongColumns(fetch.colTypes)),
					"a LONG column read as a scalar must cost the row")
			}
		})
	}
}

// TestScalarFetchIsNotOfferedTheLongReading is the same gate from the other
// side, and it is the half that says the reading is keyed on something real.
//
// The same recorded rows are re-read with the **CHAR** columns declared LONG as
// well — the three six-character scalars sitting either side of the real one.
// Every fetch must come back with nothing: the reading expects an indicator and
// a return code where the next column's value actually is, so the walk drifts
// and the row is refused. A reading that produced rows either way would say
// nothing about the recordings above.
func TestScalarFetchIsNotOfferedTheLongReading(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{goOraLongFixture, pythonThinLongFixture} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, fixture)

			for _, marker := range []string{longSQLMarker, longRawSQLMarker} {
				fetch := recordedLongFetch(t, td, marker)

				longified := make([]int, len(fetch.colTypes))
				for i := range longified {
					longified[i] = tnsTypeLONG
				}

				assert.Empty(t, fetch.rowsUnder(longified),
					"a scalar column read as a LONG must cost the row")
			}
		})
	}
}

// scalarizeLongColumns returns colTypes with every LONG and LONG RAW declared an
// ordinary VARCHAR — the reading rowValueShapeOf gave those columns before
// rowValueLongInline existed, expressed as the one type code that differs.
func scalarizeLongColumns(colTypes []int) []int {
	out := append([]int(nil), colTypes...)
	for i, t := range out {
		if t == tnsTypeLONG || t == tnsTypeLONGRAW {
			out[i] = tnsTypeVARCHAR
		}
	}

	return out
}

// recordedFetch is one fetch pulled out of a recording: the column types the
// server's describe reported, the packet its rows arrived in, and the CLR long
// form the session negotiated.
//
// It exists so a fetch can be re-read under a reading its client never asked
// for, which replayCapturedRows cannot do — that one takes the describe's word
// for the column types, which is the point of it everywhere else.
type recordedFetch struct {
	colTypes []int
	rows     []byte
	oer      oerShape
}

// rowsUnder parses the recorded rows with the column types imposed, and returns
// them as strings.
func (f recordedFetch) rowsUnder(colTypes []int) [][]string {
	parsed := parseContinuationRows(f.rows, len(f.colTypes), nil, colTypes, f.oer, lobRowLocator)

	out := make([][]string, 0, len(parsed))

	for _, row := range parsed {
		strRow := make([]string, len(row))

		for i, v := range row {
			if s, ok := v.(string); ok {
				strRow[i] = s
			}
		}

		out = append(out, strRow)
	}

	return out
}

// recordedLongFetch finds the fetch whose statement contains marker and returns
// its describe types together with the row-bearing packet behind it.
//
// The rows are taken from whichever packet carries them — a LONG in the select
// list turns row prefetch off, so they arrive in a continuation of their own,
// while an ordinary fetch puts them in the QueryResult. Both are the same row
// stream and parseContinuationRows reads either.
func recordedLongFetch(t *testing.T, td *testDump, marker string) recordedFetch {
	t.Helper()

	var (
		found   recordedFetch
		started bool
	)

	for _, pkt := range td.Packets {
		if pkt.Direction == dump.DirServerToClient && observeBigClrChunksFlag(pkt.Data) {
			found.oer.bigClrChunks = true
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
			if sql, _ := findSQLInPayload(ttcPayload); strings.Contains(sql, marker) {
				started = true
			} else if started && sql != "" {
				break // a new statement begins → this fetch is over
			}

			continue
		}

		if !started {
			continue
		}

		switch funcCode { //nolint:exhaustive // only row-bearing response codes matter here
		case TTCFuncQueryResult:
			result := decodeQueryResultV2(ttcPayload, found.oer, lobRowLocator)
			if result == nil {
				continue
			}

			found.colTypes = result.ColumnTypes

			if len(result.Rows) > 0 {
				found.rows = ttcPayload
			}
		case TTCFuncContinuation:
			if found.rows == nil {
				found.rows = ttcPayload
			}
		}
	}

	require.NotEmpty(t, found.colTypes, "the recording must describe the fetch's columns")
	require.NotNil(t, found.rows, "the recording must carry the fetch's rows")

	return found
}
