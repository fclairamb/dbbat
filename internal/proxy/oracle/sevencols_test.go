package oracle

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The seven-column corner, on the two dialects whose ROW_HEADER used to be
// located by scanning forward for the `0x07` that opens the row data.
//
// Seven is the one column count at which that scan cannot work, and the corpus
// had no fetch at it: the recorded describes carry one, eight and six columns,
// so every fixture in testdata/ agreed with a reading that was wrong. The
// header's own column-count field is the first thing behind the `06 22` marker
// the scan anchors on, and both of these dialects spell seven there as the
// single byte `0x07` — a little-endian ub4 `07 00 00 00` on the 4-byte OCI one,
// a TTC compressed int `01 07` on the compressed one. So the scan stopped on
// the count, called the byte after it the first value, and handed
// parseRowStream the middle of the header.
//
// Hence two recordings at exactly seven columns (sevenColumnQuery, one per
// dialect) and, beside them, the synthesized headers below — which are what
// says the measured landing is *where the layout puts it* rather than merely
// where this recording happened to put it.

// sevenColumnQuery is the fetch the two fixtures are recorded from. Seven is
// the whole point; the shape is otherwise as plain as a query can be, because
// anything else it exercised would be covered by a fixture that already exists.
//
// The values are their own ground truth — column i carries `i` and `vi` — so a
// reading that lands one field early produces something that is visibly not
// them rather than something merely shorter.
const sevenColumnQuery = "SELECT LEVEL AS c1, 'v' || LEVEL AS c2, LEVEL * 10 AS c3, " +
	"'w' || LEVEL AS c4, LEVEL * 100 AS c5, 'x' || LEVEL AS c6, LEVEL * 1000 AS c7 " +
	"FROM dual CONNECT BY LEVEL <= 3"

// sevenColumnSQLMarker is what picks the statement out of a recording. It has
// to be a substring of the text on the wire, so it stops at the first column.
const sevenColumnSQLMarker = "LEVEL AS c1"

// sevenColumnFixture is the compressed dialect's recording — a go-ora session
// running sevenColumnQuery. Regenerate with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraSevenColumns ./internal/proxy/oracle/
const sevenColumnFixture = "go_ora_sevencols.pcapng"

// ociSevenColumnFixture and oci64SevenColumnFixture are the same fetch on the
// two OCI dialects — the describe frames of an sqlplus session running
// sevenColumnQuery through dbbat. Only the 4-byte one is recorded so far, and
// it is the one the defect is about: the 64-bit dialect never scanned.
// Regenerate with:
//
//	ORACLE_CAPTURE_OCI_FIXTURES=1 \
//	  go test -tags integration -run TestCapture_OCISevenColumnFetchThroughDBBat ./internal/proxy/oracle/
//
// add ORACLE_TEST_OCI_CLIENT=container for the 64-bit one.
const (
	ociSevenColumnFixture   = "testdata/oci_sevencols.hex"
	oci64SevenColumnFixture = "testdata/oci64_sevencols.hex"
)

// sevenColumnRows is sevenColumnQuery's ground truth: three rows of seven
// values, spelled out rather than generated, so the expectation cannot drift
// with the code that produces it.
func sevenColumnRows() [][]string {
	return [][]string{
		{"1", "v1", "10", "w1", "100", "x1", "1000"},
		{"2", "v2", "20", "w2", "200", "x2", "2000"},
		{"3", "v3", "30", "w3", "300", "x3", "3000"},
	}
}

// TestDumpReplay_SevenColumnRows is the recorded half of the regression: a real
// seven-column fetch, replayed through the same decode path the proxy runs, on
// the compressed dialect.
//
// Under the forward scan this test is what failed — the header's `01 07` count
// stopped it four bytes in, and the "row" it then read out of the header's
// remaining fields was neither these values nor seven of anything.
func TestDumpReplay_SevenColumnRows(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, sevenColumnFixture)

	rows := replayCapturedRows(t, td, sevenColumnSQLMarker)
	want := sevenColumnRows()

	require.Len(t, rows, len(want), "every row of the seven-column fetch must be captured")

	for i, row := range rows {
		assert.Equalf(t, want[i], row, "row %d", i)
	}
}

// TestOCISevenColumnFetchCarriesItsRow is the 4-byte OCI dialect's recorded
// half, and the frame behind it is the defect written out in bytes: its
// ROW_HEADER opens `06 22 07 00 00 00`, where that `07` is the little-endian ub4
// column count and not the ROW_DATA byte the forward scan took it for. The row
// it used to swallow is 20 bytes further on.
//
// The fixture is one sqlplus session running sevenColumnQuery, recorded through
// dbbat. Its prefetch is 1, so the describe carries the first row only — which
// is all this needs, the rest being an ordinary row stream the corpus already
// covers.
func TestOCISevenColumnFetchCarriesItsRow(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, ociSevenColumnFixture)
	require.Len(t, frames, 2, "the fixture is the login probe and the seven-column query")

	ttc := extractTTCPayload(frames[1])

	marker := findBytes(ttc, []byte{ttcMsgRowHeader, wideRowHeaderFlag})
	require.Positive(t, marker, "the frame must carry a ROW_HEADER")
	assert.Equal(t, byte(ttcMsgBindOutput), ttc[marker+wideRowHeaderCountOffset],
		"the recording is only the regression it is meant to be if its column count "+
			"is the very byte a forward scan would stop on")

	result := decodeQueryResultV2(ttc, ociOERShape())
	require.NotNil(t, result)

	assert.Equal(t, []string{"C1", "C2", "C3", "C4", "C5", "C6", "C7"}, result.Columns)
	require.Len(t, result.Rows, 1, "sqlplus prefetched one row into the describe")
	assert.Equal(t, sevenColumnRows()[0], result.Rows[0])
}

// TestSevenColumnRowHeaderIsNotFoundAtItsOwnColumnCount is the synthesized half,
// and it is the one that states the defect in bytes.
//
// Each case is a ROW_HEADER built from the layout its dialect is pinned to —
// the 4-byte one's measured 22 bytes, the compressed one's six-integer walk —
// carrying a column count of seven, followed by a row of seven one-byte values.
// The old reading returned the offset just past the count (+3 on the 4-byte
// dialect, +4 on the compressed one); the measured reading returns the offset
// just past the ROW_DATA byte, and the values behind it are the row.
//
// Six and eight are here as the controls the corpus already had: the same
// header at a count whose low byte is not 0x07 is a count the forward scan
// walked straight past, so a regression that only broke seven would show up
// here as seven failing alone.
func TestSevenColumnRowHeaderIsNotFoundAtItsOwnColumnCount(t *testing.T) {
	t.Parallel()

	for _, numCols := range []int{6, 7, 8} {
		for name, tc := range map[string]struct {
			shape  oerShape
			header func(int) []byte
		}{
			"4-byte OCI": {
				shape:  oerShape{fixedWidth: true},
				header: wideRowHeaderBytes,
			},
			"compressed": {
				shape:  oerShape{},
				header: compressedRowHeaderBytes,
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				header := tc.header(numCols)
				values := oneByteRowValues(numCols)
				payload := append(append([]byte{}, header...), values...)

				assert.Equalf(t, len(header), rowDataStart(payload, numCols, tc.shape),
					"%d columns: the row must start where the header ends", numCols)

				assert.Equalf(t, len(header), fetchRowDataStart(payload, numCols, tc.shape),
					"%d columns: and the same header at offset 0 must read as a fetch", numCols)

				// The count is the check that makes a wrong landing fail closed:
				// a header for one column count offered to another yields no
				// rows rather than a row of the header's own bytes.
				assert.Equalf(t, -1, rowDataStart(payload, numCols+1, tc.shape),
					"%d columns: a header must not be read at the wrong column count", numCols)
			})
		}
	}
}

// TestSevenColumnFetchIsDecodedEndToEnd walks the synthesized seven-column
// payload through parseRowStream the way scanRowValues does, so the assertion
// is the row rather than an offset.
func TestSevenColumnFetchIsDecodedEndToEnd(t *testing.T) {
	t.Parallel()

	const numCols = 7

	for name, tc := range map[string]struct {
		shape  oerShape
		header func(int) []byte
	}{
		"4-byte OCI": {shape: oerShape{fixedWidth: true}, header: wideRowHeaderBytes},
		"compressed": {shape: oerShape{}, header: compressedRowHeaderBytes},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			payload := append(tc.header(numCols), oneByteRowValues(numCols)...)

			rows := scanRowValues(payload, numCols, nil, tc.shape)
			require.Len(t, rows, 1, "the payload carries exactly one row")
			assert.Equal(t, []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7"}, rows[0])
		})
	}
}

// wideRowHeaderBytes builds a 4-byte OCI ROW_HEADER for a numCols-column fetch:
// `0x06`, the `0x22` flag, a little-endian ub4 count, a ub2 = 0, a ub2 array
// size, three ub4s = 0, then the `0x07` that opens the rows. That is the layout
// testdata/oci_describe.hex and the sqlplus recordings are measured at, and its
// total is wideRowHeaderLen + 1.
func wideRowHeaderBytes(numCols int) []byte {
	header := make([]byte, wideRowHeaderLen+1)
	header[0] = ttcMsgRowHeader
	header[wideRowHeaderFlagOffset] = wideRowHeaderFlag
	binary.LittleEndian.PutUint32(header[wideRowHeaderCountOffset:], uint32(numCols)) //nolint:gosec // a column count
	binary.LittleEndian.PutUint16(header[8:], 1)                                      // the array size, 1 on both describe frames
	header[wideRowHeaderLen] = ttcMsgBindOutput

	return header
}

// compressedRowHeaderBytes builds the compressed dialect's ROW_HEADER for a
// numCols-column fetch: `0x06`, the `0x22` flag, then the six compressed
// integers the thin recordings carry — the count, its zero high part, the
// client's prefetch size, and three zeros — then the `0x07`.
func compressedRowHeaderBytes(numCols int) []byte {
	header := []byte{ttcMsgRowHeader, wideRowHeaderFlag}
	header = append(header, 0x01, byte(numCols)) // count
	header = append(header, 0x00)                // its high part
	header = append(header, 0x02, 0x03, 0xe8)    // prefetch 1000, as DBeaver and go-ora send
	header = append(header, 0x00, 0x00, 0x00)    // the three the corpus never varies
	header = append(header, ttcMsgBindOutput)

	return header
}

// oneByteRowValues is one row of numCols two-character values, in the
// length-prefixed form parseRowStream reads: `a1`, `a2`, … so a row read from
// the wrong offset cannot accidentally look right.
func oneByteRowValues(numCols int) []byte {
	var out []byte

	for i := 1; i <= numCols; i++ {
		out = append(out, 0x02, 'a', byte('0'+i))
	}

	return out
}
