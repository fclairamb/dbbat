package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oci64LOBFrames is the recorded LOB session — see ociLOBQuery and
// writeLOBFetchHexFixture. Three frames: sqlplus's login probe, the query's
// describe, and the packet its rows arrive in.
const oci64LOBFrames = "testdata/oci64_lob.hex"

// ociLOBFrames is the same query recorded from the **4-byte** OCI dialect — an
// Instant Client's sqlplus through dbbat, which is where testdata/oci_describe.hex
// came from. It is the fixture that says the sixteen-byte block behind a
// locator is a measurement rather than one server's habit.
const ociLOBFrames = "testdata/oci_lob.hex"

// goOraLOBFixture and goOraLOBStreamFixture are two of the thin dialect's three
// halves, and the pair is the point.
//
// go-ora's default LOB policy is *inline*: it asks for the bodies up front, the
// server sends them as ordinary column values, and the row carries no locator
// at all — which is why the compressed encoding never hit the defect the LOB
// framing was written for. `lob fetch=post` is the same query with the client
// asking for nothing, which is what gets a locator.
//
// Regenerate both with:
//
//	go test -tags capture -timeout 300s -run TestCapture_GoOraLOB ./internal/proxy/oracle/
const (
	goOraLOBFixture       = "go_ora_lob.pcapng"
	goOraLOBStreamFixture = "go_ora_lob_stream.pcapng"
)

// pythonThinLOBFixture is the third, and it is the one that says go-ora's inline
// default is go-ora's habit rather than the thin dialect's: a second,
// independently written driver with nothing configured fetches locators. Its
// locator header is not go-ora's either — it carries the LOB's own size and
// chunk size as well — which is what readCompressedLOBLocatorColumn steps over
// rather than counts. Regenerate with:
//
//	go test -tags capture -timeout 300s -run TestCapture_PythonThinLOB ./internal/proxy/oracle/
const pythonThinLOBFixture = "python_thin_lob.pcapng"

// jdbcThinLOBFixture is the fourth thin recording of the same query, and the
// third thin *driver*: Oracle's own JDBC thin, with nothing configured.
//
// It is here because JDBC thin prefetches LOB data by default
// (`oracle.jdbc.defaultLobPrefetchSize`), so "the driver asks for the bodies"
// was a documented possibility with no measurement behind it — and a define
// block dbbat could not walk would have left such a fetch on the locator
// reading and refused every row of it. See
// TestJDBCThinLOBFetchCapturesItsLocators for what it turned out to be.
// Regenerate with:
//
//	go test -tags capture -timeout 300s -run TestCapture_JDBCThinLOB ./internal/proxy/oracle/
const jdbcThinLOBFixture = "jdbc_thin_lob.pcapng"

// goOraLOBSQLMarker picks the thin recordings' statement out of the dump. It
// stops at the first column, so it is a substring of the text on the wire.
const goOraLOBSQLMarker = "'aaaaaa' AS c1"

// goOraBigLOBFixture is the fourth thin recording, and it exists for one
// number: 252, the largest value a CLR can carry behind a single length byte.
//
// Every LOB in goOraLOBQuery is shorter than that, so every inlined value in
// the recordings above arrived in the CLR's short form — which is why the
// reading was a single length byte for as long as it was. This is the same
// client, the same default policy and one CLOB of 300 characters, and what it
// says is that the long form is not the genuine LONG column's habit but the
// encoding's rule. See readInlineLongColumn. Regenerate with:
//
//	go test -tags capture -timeout 300s -run TestCapture_GoOraLOBInlineBig ./internal/proxy/oracle/
const goOraBigLOBFixture = "go_ora_lob_big.pcapng"

// goOraBigLOBQuery puts that 300-character CLOB between two ordinary columns,
// the same alternation goOraLOBQuery uses: `C2` is what says the walk came out
// of the value where the value ended.
const goOraBigLOBQuery = `SELECT 'aaaaaa' AS c1,
       TO_CLOB(RPAD('x', 300, 'x')) AS d1,
       'bbbbbb' AS c2
  FROM dual`

// goOraBigLOBSQLMarker picks it out of the dump, and names the one thing about
// it that matters so a reader of the fixture knows which query this is.
const goOraBigLOBSQLMarker = "RPAD('x', 300"

// goOraBigLOBValue is what that CLOB holds: 300 times `x`, forty-eight past the
// short form's limit.
var goOraBigLOBValue = strings.Repeat("x", 300)

// goOraBigLOBColumns is its describe.
var goOraBigLOBColumns = []columnDesc{
	{Name: "C1", Type: tnsTypeCHAR},
	{Name: "D1", Type: tnsTypeCLOB},
	{Name: "C2", Type: tnsTypeCHAR},
}

// goOraLOBQuery is ociLOBQuery with the XMLTYPE column removed and nothing else
// changed — same names, same order, same values — so the two recordings line up
// column for column.
//
// The removal is not a simplification, it is go-ora's limit: the driver has no
// coder for XMLTYPE and refuses the *describe*, so a query carrying one never
// reaches a fetch and records nothing at all. The opaque column is therefore
// out of this dialect's reach, which costs nothing the walk depends on — the
// object image's own header is read rather than measured, and already spans the
// dialects (readObjectImage). The LOB framing, which is not, is entirely here.
//
// It has no trailing semicolon because go-ora parses the text itself and reads
// one as a syntax error, where sqlplus needs it.
const goOraLOBQuery = `SELECT 'aaaaaa' AS c1,
       TO_CLOB('body') AS d1,
       'bbbbbb' AS c2,
       TO_CLOB('muchlongervalue-0123456789') AS d2,
       'cccccc' AS c3,
       TO_BLOB(UTL_RAW.CAST_TO_RAW('7a7a')) AS d3,
       'dddddd' AS c4,
       TO_NCLOB('nn') AS d4,
       'eeeeee' AS c5,
       'ffffff' AS c6,
       TO_CLOB(NULL) AS d5,
       'gggggg' AS c7
  FROM dual`

// goOraLOBColumns is goOraLOBQuery's describe: ociLOBColumns without the
// XMLTYPE, which go-ora has no coder for and refuses before a row is fetched.
var goOraLOBColumns = []columnDesc{
	{Name: "C1", Type: tnsTypeCHAR},
	{Name: "D1", Type: tnsTypeCLOB},
	{Name: "C2", Type: tnsTypeCHAR},
	{Name: "D2", Type: tnsTypeCLOB},
	{Name: "C3", Type: tnsTypeCHAR},
	{Name: "D3", Type: tnsTypeBLOB},
	{Name: "C4", Type: tnsTypeCHAR},
	{Name: "D4", Type: tnsTypeCLOB},
	{Name: "C5", Type: tnsTypeCHAR},
	{Name: "C6", Type: tnsTypeCHAR},
	{Name: "D5", Type: tnsTypeCLOB},
	{Name: "C7", Type: tnsTypeCHAR},
}

// ociLOBColumns is ociLOBQuery's describe, name and TTC type code in wire
// order. Four of the thirteen are the types this fixture exists for: CLOB
// (112), BLOB (113) and the opaque XMLTYPE (58), each sitting between two
// ordinary CHAR columns.
var ociLOBColumns = []columnDesc{
	{Name: "C1", Type: tnsTypeCHAR},
	{Name: "D1", Type: tnsTypeCLOB},
	{Name: "C2", Type: tnsTypeCHAR},
	{Name: "D2", Type: tnsTypeCLOB},
	{Name: "C3", Type: tnsTypeCHAR},
	{Name: "D3", Type: tnsTypeBLOB},
	{Name: "C4", Type: tnsTypeCHAR},
	{Name: "D4", Type: tnsTypeCLOB},
	{Name: "C5", Type: tnsTypeCHAR},
	{Name: "X1", Type: tnsTypeOPAQUE},
	{Name: "C6", Type: tnsTypeCHAR},
	{Name: "D5", Type: tnsTypeCLOB},
	{Name: "C7", Type: tnsTypeCHAR},
}

// ociLOBXMLValue is what the XMLTYPE column captures: the opaque type's own
// image, decoded — `XMLTYPE('<a/>')` read back as `<a/>`.
//
// It is deliberately *not* the placeholder the LOB columns beside it get, and
// the difference is the one fact that decides it — where the data is. A LOB's
// contents are not in this packet at all and dbbat will not go and ask for
// them, so there is nothing to render and the column says so. An opaque column
// carries its own image a few bytes further along the same row: here
// `85 01 0c 01 00000014 3c 61 2f 3e`, the flag, the length 12, the kind 0x14
// (text) and the four bytes of `<a/>`.
//
// Until decodeObjectImage this was the locator in front of it, captured as
// `0000002400220208...` — thirty-six bytes of per-fetch handle where four bytes
// of value were already in hand.
const ociLOBXMLValue = "<a/>"

// TestOCI64LOBDescribeCarriesNoRowValuesAtAll is the measurement the LOB fix
// rests on, and it is the half that was guessed wrong before the session was
// recorded: with a LOB in the select list the describe carries **no rows**.
//
// Oracle turns row prefetch off for a LOB column, so the QueryResult that
// answers the execute has its column records and nothing behind them — no
// ROW_HEADER, no values — and the whole fetch follows in a packet of its own.
// A reading that looked for the values inside the describe, or that blamed the
// scan for drifting inside it, would be looking at bytes that are not there.
func TestOCI64LOBDescribeCarriesNoRowValuesAtAll(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64LOBFrames)
	require.Len(t, frames, 3, "the fixture must carry the login probe, the describe and the fetch")

	describe := extractTTCPayload(frames[1])
	require.Equal(t, byte(TTCFuncQueryResult), describe[0], "frame 1 must be the describe")

	result := decodeQueryResultV2(describe, oci64OERShape(), lobRowLocator)
	require.NotNil(t, result)

	assert.Equal(t, ociLOBColumns, parseColumnDescribes(describe, oci64OERShape()),
		"the describe itself reads correctly — the columns and their types are all there")
	assert.Empty(t, result.Rows,
		"and it carries no row values at all: prefetch is off, so they are not in this packet")

	fetch := extractTTCPayload(frames[2])
	assert.Equal(t, byte(TTCFuncContinuation), fetch[0],
		"the values arrive in a packet that opens with the ROW_HEADER object")
	assert.Equal(t, wide64RowHeaderLen+1, fetchRowDataStart(fetch, len(ociLOBColumns), oci64OERShape()),
		"which is what fetchRowDataStart reads, at offset 0 and nowhere else")
}

// TestOCI64LOBFetchKeepsEveryOrdinaryColumn is the point of the whole spec,
// stated where it is observable: in the JSON dbbat writes to query_rows.
//
// Before this, a CLOB or an XMLTYPE anywhere in a select list cost the fetch
// **every** row — not an unreadable value for that one column, the whole row,
// with the ordinary columns beside it and with nothing in the audit trail
// saying a row had been dropped. The seven `C` columns here are that claim:
// they alternate with the LOBs, so each one is a value that only survives if
// the walk came out of the locator before it on the right byte.
//
// `C7` is last on purpose. The NULL CLOB in front of it sends a zero-length
// locator with a shorter block behind it than a real one, so a walk that
// skipped a fixed distance would land inside `C7`'s own value.
func TestOCI64LOBFetchKeepsEveryOrdinaryColumn(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64LOBFrames)
	require.Len(t, frames, 3, "the fixture must carry the login probe, the describe and the fetch")

	s, rowStore, _ := newCapturingSession(t, 10000)
	s.oer = oci64OERShape()
	require.True(t, s.oerShapeSnapshot().fixedWidth64,
		"the session must speak the encoding the frames were recorded on")

	s.trackerMu.Lock()
	s.handleQueryResultV2(extractTTCPayload(frames[1]))

	require.NotNil(t, s.tracker.pendingQuery, "the describe must have opened the fetch")
	require.Len(t, s.tracker.pendingQuery.cursor.columns, len(ociLOBColumns))

	// The fetch ends on ORA-01403, which completes the query and lets go of the
	// pending row sink — so it is taken before rather than looked for after.
	sink := s.tracker.pendingQuery.rowSink

	s.handleContinuation(extractTTCPayload(frames[2]))
	s.trackerMu.Unlock()

	sink.Flush(t.Context())

	rows := rowStore.rowData(t)
	require.Len(t, rows, 1, "the fetch carries exactly one row")

	assert.Equal(t, map[string]interface{}{
		"C1": "aaaaaa",
		"D1": "<CLOB locator>",
		"C2": "bbbbbb",
		"D2": "<CLOB locator>",
		"C3": "cccccc",
		"D3": "<BLOB locator>",
		"C4": "dddddd",
		"D4": "<CLOB locator>",
		"C5": "eeeeee",
		"X1": ociLOBXMLValue,
		"C6": "ffffff",
		"D5": "",
		"C7": "gggggg",
	}, rows[0])
}

// TestOCI64LOBFetchIsNotOfferedToTheOtherTwoEncodings is the gate every reading
// in this package carries: the encoding comes from the session, so the fetch
// must yield its row under the shape it was recorded from and nothing under
// either of the others. A packet offered three readings is a packet with three
// chances to produce a plausible-looking row.
func TestOCI64LOBFetchIsNotOfferedToTheOtherTwoEncodings(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64LOBFrames)
	fetch := extractTTCPayload(frames[2])
	types := describeColumnTypes(ociLOBColumns)

	require.Len(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oci64OERShape(), lobRowLocator), 1,
		"the fixture must yield its row under the shape it was recorded from")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, ociOERShape(), lobRowLocator),
		"a 64-bit OCI fetch must not be read as a 4-byte OCI one")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oerShape{}, lobRowLocator),
		"nor as a compressed one")
}

// TestLOBRowIsRefusedWhenTheFramingSkipLandsWrong is what makes the measured
// skip lengths safe to have got wrong.
//
// The bytes between a locator and the next column are framing, and their length
// is a measurement off one server: nothing guarantees the next Oracle release,
// or a dialect no one has recorded, spells them the same way. So a row that
// needed a skip is kept only when the columns after it come out on a row
// marker. Here the fetch is handed a column count one too high, which slides
// every skip past its own column — and the answer is no row at all, never a row
// whose values are framing bytes.
func TestLOBRowIsRefusedWhenTheFramingSkipLandsWrong(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64LOBFrames)
	fetch := extractTTCPayload(frames[2])

	// The header's own count check keeps a wrong column count from even finding
	// the row area, so the drift is introduced where the walk reads it instead:
	// the same start, one more column than the row holds.
	start := fetchRowDataStart(fetch, len(ociLOBColumns), oci64OERShape())
	require.Positive(t, start)

	drifted := len(ociLOBColumns) + 1
	types := make([]int, 0, drifted)
	types = append(types, describeColumnTypes(ociLOBColumns)...)
	types = append(types, tnsTypeCHAR)

	assert.Empty(t, parseRowStream(fetch, start, drifted, allColumns(drifted), nil, types, oci64OERShape(), lobRowLocator),
		"a walk that comes out of the framing on the wrong byte must cost the row, not fill it")
}

// TestReadObjectImageReadsTheLengthTwiceOrNotAtAll pins the one thing that
// makes the object-image header a reading rather than a pattern match: the
// image length arrives as a four-byte field and as a single byte, and only a
// header where the two agree is accepted.
func TestReadObjectImageReadsTheLengthTwiceOrNotAtAll(t *testing.T) {
	t.Parallel()

	// Twelve bytes of framing, then the header the 64-bit dialect writes ahead
	// of a 4-byte image, then the image, then the next column's length byte.
	payload := []byte{
		0x00, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0,
		0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x04,
		0xde, 0xad, 0xbe, 0xef,
		0x07,
	}

	image, next, ok := readObjectImage(payload, 0)
	require.True(t, ok)
	assert.Equal(t, len(payload)-1, next, "the resume offset must be the next column's length byte")
	assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, image,
		"and the bytes between the header and it are the image")

	// The same bytes with the one-byte length disagreeing with the four-byte
	// one: no header, no row.
	disagreeing := append([]byte(nil), payload...)
	disagreeing[18] = 0x05

	_, _, ok = readObjectImage(disagreeing, 0)
	assert.False(t, ok, "two spellings of one length that disagree are not a header")

	// And nothing at all to find within the window.
	_, _, ok = readObjectImage(make([]byte, 64), 0)
	assert.False(t, ok, "a run of zeros is not an image header")
}

// TestObjectImageDecodesOrKeepsTheLocator is the fail-closed half of the
// decode, held on synthesized images because the corpus carries only the two
// that *do* decode — one named object and one XMLTYPE, each recorded on both
// OCI dialects (TestOCI64RowCaptureCarriesTheDescribesValues,
// TestOCIRowCaptureCarriesTheDescribesColumnNames and the two LOB fetches pin
// those). What has never been recorded is every other shape, which is exactly
// what must not be guessed at.
func TestObjectImageDecodesOrKeepsTheLocator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		image []byte
		want  string
	}{
		{
			name:  "the recorded named object, attributes read with no types to read them by",
			image: []byte{0x84, 0x01, 0x08, 0x02, 0xc1, 0x02, 0x01, 0x78},
			want:  ociDescribeObjectValue,
		},
		{
			name:  "the recorded XMLTYPE, kind 0x14 and its text behind it",
			image: []byte{0x85, 0x01, 0x0c, 0x01, 0x00, 0x00, 0x00, 0x14, 0x3c, 0x61, 0x2f, 0x3e},
			want:  ociLOBXMLValue,
		},
		{
			name:  "an object with no attributes at all",
			image: []byte{0x84, 0x01, 0x03},
			want:  "()",
		},
		{
			name:  "a length the image's own header disagrees with",
			image: []byte{0x84, 0x01, 0x09, 0x02, 0xc1, 0x02, 0x01, 0x78},
			want:  "",
		},
		{
			name:  "an attribute walk that overruns the image",
			image: []byte{0x84, 0x01, 0x08, 0x02, 0xc1, 0x02, 0x04, 0x78},
			want:  "",
		},
		{
			name:  "an attribute walk that leaves a remainder is the same refusal",
			image: []byte{0x84, 0x01, 0x08, 0x02, 0xc1, 0x02, 0x00, 0x78},
			want:  "",
		},
		// The two CLR forms the attribute walk refuses. Both images declare
		// their own length correctly — 0x06 for six bytes — on purpose: a
		// length that disagreed would be refused by the self-length check
		// before the walk ever ran, and the sub-test would pass while pinning
		// nothing. That is how the first version of this case was wrong.
		{
			name:  "a NULL attribute, which has never been recorded",
			image: []byte{0x84, 0x01, 0x06, 0xff, 0x01, 0x78},
			want:  "",
		},
		{
			name:  "a chunked attribute, reached after one that reads cleanly",
			image: []byte{0x84, 0x01, 0x06, 0x01, 0x78, 0xfe},
			want:  "",
		},
		{
			name:  "the collection flag, which nothing in the corpus carries",
			image: []byte{0x88, 0x01, 0x08, 0x02, 0xc1, 0x02, 0x01, 0x78},
			want:  "",
		},
		{
			name:  "an opaque kind that is a LOB locator, so the data is not here either",
			image: []byte{0x85, 0x01, 0x0c, 0x01, 0x00, 0x00, 0x00, 0x11, 0x3c, 0x61, 0x2f, 0x3e},
			want:  "",
		},
		{
			name:  "an opaque image too short to carry a kind",
			image: []byte{0x85, 0x01, 0x05, 0x01, 0x00},
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := decodeObjectImage(tc.image)
			assert.Equal(t, tc.want != "", ok,
				"an image dbbat has not measured must be refused, not rendered")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestUnreadableObjectImageKeepsTheLocatorHex is the refusal above seen where it
// matters: in the value the column captures. A row whose object image does not
// decode keeps every other column and falls back to the locator's own bytes —
// the value this package captured before the image was read at all — rather
// than losing the row or inventing an object.
func TestUnreadableObjectImageKeepsTheLocatorHex(t *testing.T) {
	t.Parallel()

	// A two-byte locator, twelve bytes of framing, then the header and an image
	// carrying the collection flag decodeObjectImage refuses.
	payload := []byte{
		0x02, 0xab, 0xcd,
		0x00, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0,
		0x03, 0x00, 0x00, 0x00, 0x01, 0x00, 0x03,
		0x88, 0x01, 0x03,
		0x07,
	}

	value, next, ok := readRowColumn(payload, 0, oci64OERShape(), []int{tnsTypeNamedObject}, 0, lobRowLocator)
	require.True(t, ok, "the column must still be stepped over — the row is not lost")
	assert.Equal(t, len(payload)-1, next)
	assert.Equal(t, "abcd", value, "and the capture falls back to the locator hex")
}
