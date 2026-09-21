package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ociDescribes is every describe response of a recorded sqlplus session. See
// capture_oci_describe_test.go.
const ociDescribes = "testdata/oci_describe.hex"

// TestOCIDescribeRecordsParse is what the fixed-width column record is pinned
// against, and it is pinned against *values* rather than against alignment.
//
// Until this existed, parseColumnDescribes read TTC compressed integers only, so
// an OCI session's describes never parsed and its column names came from the
// heuristic scanner — which guesses at what a describe record spells out. The
// last describe in the fixture is a deliberately awkward one: a
// `VARCHAR2(4000)` whose maximum length does not fit a byte, a `NUMBER(10,2)`
// with a real precision and scale, `1/3` whose scale is the -127 float
// sentinel, temporal types, a `CHAR(5)`, a `RAW`, and an object column whose
// record carries a non-null 16-byte type OID.
//
// That last column is the one that settles a question no run of zeros could: the
// type OID arrives as a four-byte little-endian length of 16 followed by a CLR,
// which is what says the field is a DLC in this encoding too — and therefore
// what fixes how many bytes the two integers before it may occupy.
func TestOCIDescribeRecordsParse(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, ociDescribes)

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)
		require.Equalf(t, byte(TTCFuncQueryResult), ttc[0], "frame %d must be a describe", i)

		assert.NotEmptyf(t, parseColumnDescribes(ttc, true),
			"every describe an OCI session receives must parse under the fixed-width reading: frame %d", i)
	}

	cols := parseColumnDescribes(extractTTCPayload(frames[len(frames)-1]), true)

	assert.Equal(t, []columnDesc{
		{Name: "N2", Type: tnsTypeNUMBER},
		{Name: "BIG", Type: tnsTypeVARCHAR},
		{Name: "FLT", Type: tnsTypeNUMBER},
		{Name: "D", Type: tnsTypeDATE},
		{Name: "TS", Type: tnsTypeTSTZDTY},
		{Name: "C5", Type: tnsTypeCHAR},
		{Name: "R", Type: tnsTypeRAW},
		{Name: "OBJ", Type: ociObjectColumnType},
	}, cols)
}

// ociObjectColumnType is the TTC type code 23ai reports for an object column in
// a describe. go-ora names no constant for it — its enum stops at OCIRef (110)
// and picks up again at JSON (119) — but isKnownTNSType's 60..127 range covers
// it, which is the alignment proof the walk actually relies on.
const ociObjectColumnType = 121

// TestOCIDescribeIsNotOfferedToAThinSession is the same gate the REF-cursor walk
// carries, on the describe path: the encoding comes from the session, so an OCI
// describe must yield nothing under the compressed reading and vice versa. A
// payload offered both layouts is a payload with two chances to produce a
// plausible-looking column list.
func TestOCIDescribeIsNotOfferedToAThinSession(t *testing.T) {
	t.Parallel()

	oci := extractTTCPayload(recordedFrames(t, ociDescribes)[0])

	assert.NotEmpty(t, parseColumnDescribes(oci, true),
		"the fixture must parse under the shape it was recorded from")
	assert.Nil(t, parseColumnDescribes(oci, false),
		"an OCI describe must not be read as a compressed one")
}

// ociExpressionColumnName is the login probe's one column: an *expression*, so
// its name is the whole 67-character expression text and there is nothing in the
// payload for a heuristic scanner to recognize as an identifier. The scanner
// found no name at all for it, which is what made every row of that fetch land
// in query_rows as an empty JSON object.
const ociExpressionColumnName = `DECODE(USER,'XS$NULL',XS_SYS_CONTEXT('XS$SESSION','USERNAME'),USER)`

// TestOCIRowCaptureCarriesTheDescribesColumnNames is the payoff of reading the
// records, stated where it is actually visible: in the JSON dbbat writes to
// query_rows.
//
// Both frames come off a recorded sqlplus session and both carry a row, so the
// whole path runs — handleQueryResultV2 reads the describe under the session's
// learned encoding, hangs the columns on the cursor, and captureRow keys the row
// by their names.
//
// The scanner's own answer is asserted alongside, because "real names" only
// means something next to what it produced. It is not a near miss on either
// frame: on the login probe it yields nothing at all, and on the eight-column
// describe it drops the two one-character names (`D`, `R`) and invents two that
// are not columns — the schema `SYSTEM` and the object type `DBBAT_CAP_OBJ`,
// both of which live in the record as the *other* two DLCs behind the name. A
// row captured under those keys is not merely unlabeled; six of its eight values
// are filed under the wrong column.
func TestOCIRowCaptureCarriesTheDescribesColumnNames(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, ociDescribes)
	require.GreaterOrEqual(t, len(frames), 2, "the fixture must carry both describes")

	tests := []struct {
		name    string
		frame   int
		want    map[string]interface{}
		scanned []string
	}{
		{
			name:    "an expression column the scanner cannot see at all",
			frame:   0,
			want:    map[string]interface{}{ociExpressionColumnName: "SYSTEM"},
			scanned: nil,
		},
		{
			name:  "eight columns, two of them one character long",
			frame: 1,
			want: map[string]interface{}{
				"N2":  "1",
				"BIG": "x",
				"FLT": "0.3333333333333333333333333333333333333333",
				"D":   "2026-09-20 20:11:58",
				"TS":  "2026-09-20 20:11:58.527544 +00:00",
				"C5":  "ab   ",
				"R":   "7a7a",
				"OBJ": "00000024002202085bf0bee82b6b0146e06303d7a8c0ea1b000000000000000000000000",
			},
			scanned: []string{"N2", "BIG", "FLT", "TS", "C5", "OBJ", "SYSTEM", "DBBAT_CAP_OBJ"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ttc := extractTTCPayload(frames[tc.frame])

			// What the heuristic scanner makes of the very same bytes.
			assert.Equal(t, tc.scanned, scanAndPadColumnNames(ttc),
				"the scanner's answer is the baseline this test exists to replace")

			s, rowStore, _ := newCapturingSession(t, 10000)
			s.oer = ociOERShape()
			require.True(t, s.oerShapeSnapshot().fixedWidth,
				"the session must speak the encoding the frame was recorded on")

			s.trackerMu.Lock()
			s.handleQueryResultV2(ttc)
			s.trackerMu.Unlock()

			require.NotNil(t, s.tracker.pendingQuery, "the fetch must still be open")
			assert.Equal(t, describeColumnNames(parseColumnDescribes(ttc, true)),
				columnNamesOf(s.tracker.pendingQuery.cursor.columns),
				"the cursor must carry the describe's own names")

			s.tracker.pendingQuery.rowSink.Flush(t.Context())

			rows := rowStore.rowData(t)
			require.Len(t, rows, 1, "the frame carries exactly one row")
			assert.Equal(t, tc.want, rows[0])
		})
	}
}

// columnNamesOf is the name list of a cursor's learned columns.
func columnNamesOf(cols []columnDef) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}

	return out
}
