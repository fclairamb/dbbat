package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The other two dialects' half of the LOB evidence.
//
// The framing behind a LOB locator was measured off one recording — sqlplus on
// the 64-bit OCI dialect — and turned into two package constants. These are the
// same query (ociLOBQuery / goOraLOBQuery) recorded on the two dialects that
// had no sample at all, and between them they say the constants were a sum
// rather than a measurement:
//
//   - the **4-byte OCI** dialect sends its `size` field as a TTC compressed
//     integer where the 64-bit one sends a fixed ub8, so its LOB column is six
//     bytes shorter and every row of such a fetch used to be refused;
//   - the **thin** dialect, under the LOB policy a thin client defaults to,
//     sends no locator at all — the server inlines the LOB's own bytes.
//
// See readFixedLOBColumn and readCompressedLOBColumn.

// TestOCILOBFetchKeepsEveryOrdinaryColumn is
// TestOCI64LOBFetchKeepsEveryOrdinaryColumn on the other OCI dialect, and it is
// the test that would have failed the day the sixteen-byte skip was written:
// the same thirteen columns, the same five LOBs, the same alternation of
// six-character scalars saying where each value begins — and a locator whose
// header is two bytes shorter.
//
// `X1` is the one field the two dialects do agree on, byte for byte
// (ociLOBXMLValue), which is the other half of the same point: the object
// image's framing is *read* rather than counted, so it spans both already.
func TestOCILOBFetchKeepsEveryOrdinaryColumn(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, ociLOBFrames)
	require.Len(t, frames, 3, "the fixture must carry the login probe, the describe and the fetch")

	s, rowStore, _ := newCapturingSession(t, 10000)
	s.oer = ociOERShape()
	require.True(t, s.oerShapeSnapshot().fixedWidth,
		"the session must speak the encoding the frames were recorded on")
	require.False(t, s.oerShapeSnapshot().fixedWidth64,
		"and it must be the 4-byte one, which is the whole point of this fixture")

	s.trackerMu.Lock()
	s.handleQueryResultV2(extractTTCPayload(frames[1]))

	require.NotNil(t, s.tracker.pendingQuery, "the describe must have opened the fetch")
	require.Len(t, s.tracker.pendingQuery.cursor.columns, len(ociLOBColumns))

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

// TestOCILOBFetchIsNotOfferedToTheOtherTwoEncodings is the gate every reading in
// this package carries, on the fixture above: a packet offered three readings is
// a packet with three chances to produce a plausible-looking row.
//
// It is worth more here than on most fixtures, because the two OCI dialects'
// LOB headers differ by exactly two bytes — the kind of near-miss that a walk
// with a tolerance instead of a measurement would read either way.
func TestOCILOBFetchIsNotOfferedToTheOtherTwoEncodings(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, ociLOBFrames)
	fetch := extractTTCPayload(frames[2])
	types := describeColumnTypes(ociLOBColumns)

	require.Len(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, ociOERShape(), lobRowLocator), 1,
		"the fixture must yield its row under the shape it was recorded from")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oci64OERShape(), lobRowLocator),
		"a 4-byte OCI fetch must not be read as a 64-bit one")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oerShape{}, lobRowLocator),
		"nor as a compressed one")
}

// TestThinLOBFetchCarriesTheContentInsteadOfALocator is the thin dialect's
// recording, and the finding it exists for is in its expectation rather than in
// any assertion about bytes: `D1` is `body`, not `<CLOB locator>`.
//
// go-ora asks for the LOB bodies up front by default, so the server inlines
// them and the row carries no handle to step over at all. That is why the
// compressed encoding never showed the defect the LOB framing was written
// for — and why it had one of its own: the walk was skipping sixteen bytes past
// a value that ended where it said it did, and losing every such row.
//
// The ask is a define block re-declaring each LOB column as a LONG, which is
// what execDefineLOBShape reads here — off the client's own frame, before the
// rows arrive. Nothing in the server's frames says it: this fixture's column
// records are byte-identical to the streamed one's below.
//
// `D4` is an NCLOB, so what arrives is the UCS-2 the server sent — `00 6e 00
// 6e` — and decodeOracleRawValue renders a byte run with NULs in it as hex
// rather than as text, which is the rule it has always applied and not one this
// fixture asks for an exception to.
//
// `X1` has no counterpart here: go-ora has no coder for XMLTYPE and refuses the
// describe, so the opaque column is out of the thin dialect's reach entirely.
func TestThinLOBFetchCarriesTheContentInsteadOfALocator(t *testing.T) {
	t.Parallel()

	require.Contains(t, goOraLOBQuery, goOraLOBSQLMarker,
		"the marker has to be a substring of the statement on the wire, or it selects nothing")

	rows := replayCapturedRows(t, loadTestDump(t, goOraLOBFixture), goOraLOBSQLMarker)
	require.Len(t, rows, 1, "the query selects from dual and returns exactly one row")
	require.Len(t, rows[0], len(goOraLOBColumns), "every column of the describe must come back")

	assert.Equal(t, []string{
		"aaaaaa",
		"body",
		"bbbbbb",
		"muchlongervalue-0123456789",
		"cccccc",
		"7a7a",
		"dddddd",
		"006e006e",
		"eeeeee",
		"ffffff",
		"",
		"gggggg",
	}, rows[0])
}

// goOraLOBLocatorRow is what the two locator recordings capture: the ordinary
// columns verbatim, each LOB a placeholder naming its type, and the NULL CLOB
// the empty string every other NULL captures as.
//
// It is deliberately the same expectation for both, because the point of having
// two is that the *column* reads the same however the client spelled the header
// in front of the locator.
var goOraLOBLocatorRow = []string{
	"aaaaaa",
	"<CLOB locator>",
	"bbbbbb",
	"<CLOB locator>",
	"cccccc",
	"<BLOB locator>",
	"dddddd",
	"<CLOB locator>",
	"eeeeee",
	"ffffff",
	"",
	"gggggg",
}

// TestThinStreamedLOBFetchCapturesItsLocators is the same client asking for
// locators instead of bodies (`lob fetch=post`), and it is the half that used
// to cost the whole fetch.
//
// The column is framed a third way again — the locator's size and then the
// locator, where the inline shape is a value and its indicator pair — and
// **nothing in the describe tells the two apart**: the column records of the two
// recordings are identical, because the difference was asked for on the client's
// side of the wire. So it is asked for there that dbbat reads it. This session
// sends no define at all, which is the ask for nothing, and a locator is what
// the server sends when nothing was asked.
//
// The captured values are placeholders rather than data, and that is the rule
// the inline half reads the other way round: a locator names a LOB inside the
// server and its contents are not in this packet, so there is nothing to
// render. See lobLocatorPlaceholder.
func TestThinStreamedLOBFetchCapturesItsLocators(t *testing.T) {
	t.Parallel()

	rows := replayCapturedRows(t, loadTestDump(t, goOraLOBStreamFixture), goOraLOBSQLMarker)
	require.Len(t, rows, 1, "the query selects from dual and returns exactly one row")

	assert.Equal(t, goOraLOBLocatorRow, rows[0])
}

// TestPythonThinLOBFetchCapturesItsLocators is the same query on a second,
// independently written thin driver with **nothing configured**, and it is what
// makes the reading above a dialect's rather than one driver's switch.
//
// python-oracledb thin fetches LOB handles by default, so its rows carry
// locators — and it does send a define, keeping each LOB column the LOB type it
// already was. Two of the three thin recordings therefore ask for locators, and
// go-ora's inline default is go-ora's habit rather than the thin norm, which is
// why the unlearned reading is the locator.
//
// Its locator header is not go-ora's: python asks for the LOB's own size and
// chunk size as well, which is the OCI dialects' four-field header spelled in
// compressed integers. The walk does not count those fields, it steps over them
// until the size it read agrees with the locator's own length byte — see
// readCompressedLOBLocatorColumn — so the column reads the same either way, and
// that is exactly what this asserts.
func TestPythonThinLOBFetchCapturesItsLocators(t *testing.T) {
	t.Parallel()

	rows := replayCapturedRows(t, loadTestDump(t, pythonThinLOBFixture), goOraLOBSQLMarker)
	require.Len(t, rows, 1, "the query selects from dual and returns exactly one row")

	assert.Equal(t, goOraLOBLocatorRow, rows[0])
}
