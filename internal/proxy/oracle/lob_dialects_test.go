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
// (ociLOBXMLLocator), which is the other half of the same point: the object
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
		"X1": ociLOBXMLLocator,
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

	require.Len(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, ociOERShape()), 1,
		"the fixture must yield its row under the shape it was recorded from")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oci64OERShape()),
		"a 4-byte OCI fetch must not be read as a 64-bit one")
	assert.Empty(t, parseContinuationRows(fetch, len(ociLOBColumns), nil, types, oerShape{}),
		"nor as a compressed one")
}

// TestThinLOBFetchCarriesTheContentInsteadOfALocator is the thin dialect's
// recording, and the finding it exists for is in its expectation rather than in
// any assertion about bytes: `D1` is `body`, not `<CLOB locator>`.
//
// A thin client asks for the LOB bodies up front by default, so the server
// inlines them and the row carries no handle to step over at all. That is why
// the compressed encoding never showed the defect the LOB framing was written
// for — and why it had one of its own: the walk was skipping sixteen bytes past
// a value that ended where it said it did, and losing every such row.
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

// TestThinStreamedLOBFetchIsRefusedRatherThanGuessed is the honest half, and it
// pins a limit rather than a capability.
//
// The same thin client asked for locators instead of bodies (`lob fetch=post`)
// frames the column a third way again — two CLRs, where the inline shape is a
// CLR and two compressed integers — and **nothing in the describe tells the two
// apart**: the column records of the two recordings are identical, because the
// difference was asked for in the execute's options. So the row is read under
// the shape a thin client defaults to, it comes out on the wrong byte under the
// other one, and rowEndsAtMarker costs it the row.
//
// That is the behavior this fixture is here to hold still. Capturing it would
// need the client's own execute read for its LOB policy, which is a piece of
// work of its own and not one to fake with a second guess at the row —
// specs/todos/2026-09-22-09-oracle-a-thin-client-that-asks-for-lob-locators-loses-its-rows.md.
func TestThinStreamedLOBFetchIsRefusedRatherThanGuessed(t *testing.T) {
	t.Parallel()

	rows := replayCapturedRows(t, loadTestDump(t, goOraLOBStreamFixture), goOraLOBSQLMarker)
	assert.Empty(t, rows,
		"a walk that comes out of the framing on the wrong byte must cost the row, not fill it")
}
