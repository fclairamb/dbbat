package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oci64LOBFrames is the recorded LOB session — see ociLOBQuery and
// writeLOBFetchHexFixture. Three frames: sqlplus's login probe, the query's
// describe, and the packet its rows arrive in.
const oci64LOBFrames = "testdata/oci64_lob.hex"

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

// ociLOBXMLLocator is what the XMLTYPE column captures: the opaque locator's
// own bytes, the same value an object column has always captured.
//
// It is deliberately *not* the placeholder the LOB columns get, and the
// difference is the one fact that decides it — where the data is. A LOB's
// contents are not in this packet at all and dbbat will not go and ask for
// them, so there is nothing to render and the column says so. An opaque or
// object column carries its own image a few bytes further along the same row,
// so replacing the column wholesale with a marker would be discarding bytes
// dbbat is holding. Reading that image instead of the locator is a better
// value than either and is filed as its own piece of work.
const ociLOBXMLLocator = "000000240022020800000000000000000000000000020100000000000000000000000000"

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

	result := decodeQueryResultV2(describe, oci64OERShape())
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
		"X1": ociLOBXMLLocator,
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

	require.Len(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oci64OERShape()), 1,
		"the fixture must yield its row under the shape it was recorded from")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, ociOERShape()),
		"a 64-bit OCI fetch must not be read as a 4-byte OCI one")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oerShape{}),
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

	assert.Empty(t, parseRowStream(fetch, start, drifted, allColumns(drifted), nil, types),
		"a walk that comes out of the framing on the wrong byte must cost the row, not fill it")
}

// TestSkipObjectImageReadsTheLengthTwiceOrNotAtAll pins the one thing that
// makes the object-image skip a reading rather than a pattern match: the image
// length arrives as a four-byte field and as a single byte, and only a header
// where the two agree is accepted.
func TestSkipObjectImageReadsTheLengthTwiceOrNotAtAll(t *testing.T) {
	t.Parallel()

	// Twelve bytes of framing, then the header the 64-bit dialect writes ahead
	// of a 4-byte image, then the image, then the next column's length byte.
	payload := []byte{
		0x00, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0,
		0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x04,
		0xde, 0xad, 0xbe, 0xef,
		0x07,
	}

	next, ok := skipObjectImage(payload, 0)
	require.True(t, ok)
	assert.Equal(t, len(payload)-1, next, "the skip must land on the next column's length byte")

	// The same bytes with the one-byte length disagreeing with the four-byte
	// one: no header, no row.
	disagreeing := append([]byte(nil), payload...)
	disagreeing[18] = 0x05

	_, ok = skipObjectImage(disagreeing, 0)
	assert.False(t, ok, "two spellings of one length that disagree are not a header")

	// And nothing at all to find within the window.
	_, ok = skipObjectImage(make([]byte, 64), 0)
	assert.False(t, ok, "a run of zeros is not an image header")
}
