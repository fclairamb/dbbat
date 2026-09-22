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

// TestOCILongFetchKeepsEveryOrdinaryColumn is the OCI half, and it is the
// measurement the thin recordings could not stand in for: **every field** of a
// LONG column is spelled differently there. The chunk lengths are ub4s where
// thin sends compressed integers, the indicator and return code are ub2s where
// thin sends two more of them, and a NULL spells the pair `ffff 7d05` — the
// same -1 and 1403, in OCI's own encoding.
//
// Both dialects, because their *LOB* columns differ from each other by six
// bytes and nothing said a LONG one would not. It does not: the two fixtures
// carry the same bytes here, which makes this the one column shape in this
// package with a single OCI reading rather than two.
func TestOCILongFetchKeepsEveryOrdinaryColumn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fixture string
		shape   oerShape
		want    [][]string
	}{
		{ociLongFrames, ociOERShape(), longRows},
		{oci64LongFrames, oci64OERShape(), longRows},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			frames := recordedFrames(t, tc.fixture)
			require.Len(t, frames, 6,
				"the fixture must carry the login probe, then a describe and its rows for each query")

			// The same expectations the thin recordings are pinned against,
			// deliberately: what a column captures is not allowed to depend on
			// which client asked for it. No exception now on either dialect —
			// sqlplus fetches this result set over two round trips, and the
			// **second** packet's ROW_HEADER carries flag 0x02 where the first
			// carries 0x22, which isRowHeaderFlag reads as the same object.
			// Until it did, the 64-bit reading refused that packet and the
			// 25-byte fallback scan could not reach past its 50-byte header, so
			// the NULL row below was lost — on every multi-packet 64-bit OCI
			// fetch, whatever its columns.
			assert.Equal(t, tc.want,
				ociLongFetchRows(t, tc.shape, longColumns, frames[1], frames[2], frames[3]))
			assert.Equal(t, longRawRows,
				ociLongFetchRows(t, tc.shape, longRawColumns, frames[4], frames[5]))
		})
	}
}

// TestOCIRowHeaderFlagIsTwoStatesAndNothingElse is the negative half of the
// widening above, and the reason the flag is read through a mask rather than
// simply dropped.
//
// Both round trips of the recorded fetch are taken in turn — the first packet's
// 0x22 and the second's 0x02 — and every one of the 256 values that byte could
// hold is substituted into the real header. Exactly two of them may locate the
// ROW_DATA byte, on both dialects. A reading that had stopped checking the flag
// would accept all 256 and would be a forward scan with extra steps.
func TestOCIRowHeaderFlagIsTwoStatesAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fixture   string
		shape     oerShape
		flagAt    int
		headerLen int
	}{
		{ociLongFrames, ociOERShape(), wideRowHeaderFlagOffset, wideRowHeaderLen},
		{oci64LongFrames, oci64OERShape(), wide64RowHeaderFlagOffset, wide64RowHeaderLen},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			frames := recordedFrames(t, tc.fixture)
			require.Len(t, frames, 6)

			numCols := len(longColumns)

			// frame 2 is the fetch's first packet, frame 3 the round trip after
			// it — the pair this whole reading is measured against.
			for frame, want := range map[int]byte{2: wideRowHeaderFlag, 3: 0x02} {
				payload := extractTTCPayload(frames[frame])

				require.Equal(t, want, payload[tc.flagAt],
					"frame %d must carry the flag state it was recorded with", frame)
				require.Equal(t, tc.headerLen+1, fetchRowDataStart(payload, numCols, tc.shape),
					"and its header must locate its own ROW_DATA byte, both states alike")

				var accepted []byte

				for b := range 256 {
					mutated := append([]byte(nil), payload...)
					mutated[tc.flagAt] = byte(b)

					if fetchRowDataStart(mutated, numCols, tc.shape) >= 0 {
						accepted = append(accepted, byte(b))
					}
				}

				assert.Equal(t, []byte{0x02, wideRowHeaderFlag}, accepted,
					"only the two measured flag states may locate a ROW_HEADER (frame %d)", frame)
			}
		})
	}
}

// TestOCILongFetchIsNotOfferedTheThinReading is the gate across the two
// spellings, and here it is worth more than usual: both start on the value's
// own bytes, so a walk offered either has two plausible-looking places to
// finish a column.
//
// Each OCI fetch is re-read as a thin one and must come back with nothing — the
// compressed reading takes the ub4 chunk length's first byte for a whole field
// and comes out on the wrong byte, where rowEndsAtMarker costs it the row.
func TestOCILongFetchIsNotOfferedTheThinReading(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fixture string
		shape   oerShape
	}{
		{ociLongFrames, ociOERShape()},
		{oci64LongFrames, oci64OERShape()},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			frames := recordedFrames(t, tc.fixture)
			types := describeColumnTypes(longColumns)
			fetch := extractTTCPayload(frames[2])

			require.NotEmpty(t, parseContinuationRows(fetch, len(types), nil, types, tc.shape, lobRowLocator),
				"the fixture must yield its row under the shape it was recorded from")

			for _, wrong := range []oerShape{{}, {bigClrChunks: true}} {
				assert.Empty(t, parseContinuationRows(fetch, len(types), nil, types, wrong, lobRowLocator),
					"an OCI LONG column must not be read as a thin one")
			}
		})
	}
}

// ociLongFetchRows reads one recorded OCI fetch — its describe and the packets
// its rows arrive in — and returns the rows as strings.
//
// The column types come off the recorded describe rather than from want, and
// are checked against it: a fixture whose columns Oracle did not report as LONG
// would prove nothing, and that is the one thing these recordings exist to
// establish.
//
// Each row packet is parsed on its own, threading the previous row the way a
// session does, because sqlplus fetches this result set over several round
// trips — the client FETCH between them is not in a server-frames fixture.
func ociLongFetchRows(
	t *testing.T, shape oerShape, want []columnDesc, describe []byte, fetches ...[]byte,
) [][]string {
	t.Helper()

	descs := parseColumnDescribes(extractTTCPayload(describe), shape)
	require.NotNil(t, descs, "the recorded describe must decode")
	require.Equal(t, want, descs, "and report the columns the query selected, LONG type code included")

	types := describeColumnTypes(descs)

	var (
		prev []string
		out  [][]string
	)

	for _, fetch := range fetches {
		for _, row := range parseContinuationRows(
			extractTTCPayload(fetch), len(types), prev, types, shape, lobRowLocator,
		) {
			strRow := make([]string, len(row))

			for i, v := range row {
				if s, ok := v.(string); ok {
					strRow[i] = s
				}
			}

			out = append(out, strRow)
			prev = strRow
		}
	}

	return out
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
	lob      lobRowShape
}

// rowsUnder parses the recorded rows with the column types imposed, and returns
// them as strings.
func (f recordedFetch) rowsUnder(colTypes []int) [][]string {
	parsed := parseContinuationRows(f.rows, len(f.colTypes), nil, colTypes, f.oer, f.lob)

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

			// The same half of the client leg replayCapturedRowsUnder reads: a
			// LOB column's reading is stated in a define block and nowhere else.
			// A fetch with no LOB column in it never reaches this.
			if started && len(found.colTypes) > 0 {
				if shape, ok := execDefineLOBShape(ttcPayload, found.colTypes); ok {
					found.lob = shape
				}
			}

			continue
		}

		if !started {
			continue
		}

		switch funcCode { //nolint:exhaustive // only row-bearing response codes matter here
		case TTCFuncQueryResult:
			result := decodeQueryResultV2(ttcPayload, found.oer, found.lob)
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
