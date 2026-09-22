package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gate that makes every framed column reading in this package safe to get
// wrong, tested on its own rather than only through the recordings.
//
// rowEndsAtMarker is what turns a walk that drifted into a *refused row* instead
// of a row full of framing bytes, so every byte it accepts is a byte a drifted
// walk may land on and get away with. Three of those are markers and cost
// nothing to reason about. The fourth — the summary object that ends the call —
// is a whole decoded object, and these are the tests that say where its bounds
// are, in both directions.
//
// The corpus covers the positive side incidentally (four recordings of the LONG
// queries, three of which end a row area this way; see
// TestThinLongFetchCapturesItsValue and TestOCILongFetchKeepsEveryOrdinaryColumn).
// What it cannot cover is the negative side: no recording contains a summary
// object that must be *refused* there, because a real server does not put one
// behind a row it is still sending. So they are synthesized.

// compressedSummaryObject builds the thin dialect's summary object: the 0x04
// marker and the seven leading fields as TTC compressed integers, in the order
// decodeOERFieldsAt reads them.
func compressedSummaryObject(callStatus, seq, rowNum, errNum, cursorID int) []byte {
	out := make([]byte, 0, 1+7*2)
	out = append(out, byte(TTCFuncOERR))

	for _, field := range []int{callStatus, seq, rowNum, errNum, 0, 0, cursorID} {
		out = append(out, ttcCompressedUint(uint64(field))...)
	}

	return out
}

// fixedSummaryObject builds an OCI dialect's summary object through the
// package's own encoder, so the bytes are the ones dbbat itself writes for that
// shape rather than a hand-cut approximation of them.
func fixedSummaryObject(shape oerShape, errCode, cursorID int) []byte {
	return encodeOERFixedWidth(shape, oerSummary{
		CallStatus: 1,
		SeqNumber:  10,
		CursorID:   cursorID,
		ErrorCode:  errCode,
	})
}

// TestRowEndsAtMarker_AcceptsTheFourThingsThatCanFollowARow pins the accepting
// half, including the one that is a decoded object rather than a byte.
//
// The fixed-width case carries `errNum 0` on purpose: that is what sqlplus
// sends behind the last row of a fetch (`callStatus 1, errNum 0, cursorID 2`),
// measured in testdata/oci_long.hex, so an end-of-data-only rule would refuse
// the very object this exists to accept.
func TestRowEndsAtMarker_AcceptsTheFourThingsThatCanFollowARow(t *testing.T) {
	t.Parallel()

	endOfData := compressedSummaryObject(1, 31, 2, oraNoDataFound, 9)

	info, _ := decodeOERFieldsAt(endOfData, 0)
	require.NotNil(t, info, "the synthesized object must decode as one")
	require.Equal(t, oraNoDataFound, info.ErrorCode, "and report end of data")

	for _, tc := range []struct {
		name    string
		shape   oerShape
		payload []byte
	}{
		{"row separator", thinOERShape(), []byte{0x07}},
		{"compression descriptor", thinOERShape(), []byte{continuationDescriptorMarker}},
		{"end-of-rows footer", thinOERShape(), []byte{0x08, 0x01, 0x06}},
		{"end of payload", thinOERShape(), []byte{}},
		{"thin end-of-data object", thinOERShape(), endOfData},
		{"4-byte OCI success object", ociOERShape(), fixedSummaryObject(ociOERShape(), 0, 2)},
		{"64-bit OCI success object", oci64OERShape(), fixedSummaryObject(oci64OERShape(), 0, 2)},
		{"4-byte OCI end-of-data object", ociOERShape(), fixedSummaryObject(ociOERShape(), oraNoDataFound, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.True(t, rowEndsAtMarker(tc.shape, tc.payload, 0))
		})
	}
}

// TestRowEndsAtMarker_RefusesWhatIsNotAnEnding is the half that matters, and
// the one the corpus cannot supply.
//
// Accepting an arbitrary summary object would be worse than the bug this
// terminator was widened to fix: a refused row is a row lost loudly enough to
// show up as an empty capture, while a drifted walk that lands on something
// accepted here is a **wrong row** presented as a measurement. So the two
// encodings carry different proofs, and each is tested against what it must
// keep out:
//
//   - the fixed-width layout's self-validation (errNum repeated as the RetCode)
//     is satisfied by an object reporting a real failure, so plausibleStatusOER
//     is what keeps an ORA-01722 out — a summary object reporting an error
//     assigns no cursor and never ends a row area;
//   - the compressed reading is weak enough that a run of zeroes decodes as a
//     plausible success, so it demands ORA-01403 outright.
func TestRowEndsAtMarker_RefusesWhatIsNotAnEnding(t *testing.T) {
	t.Parallel()

	const oraInvalidNumber = 1722 // a real failure: ORA-01722, "invalid number"

	errorObject := fixedSummaryObject(ociOERShape(), oraInvalidNumber, 2)

	info, _ := decodeOERFixedFieldsAt(ociOERShape(), errorObject, 0)
	require.NotNil(t, info,
		"the error-carrying object must still satisfy the layout invariant, or it proves nothing")
	require.Equal(t, oraInvalidNumber, info.ErrorCode)

	for _, tc := range []struct {
		name    string
		shape   oerShape
		payload []byte
	}{
		{"an ordinary value length", thinOERShape(), []byte{0x06, 'a', 'b', 'c'}},
		{"a bare marker byte", ociOERShape(), []byte{byte(TTCFuncOERR)}},
		{"4-byte OCI object reporting an error", ociOERShape(), errorObject},
		{
			"64-bit OCI object reporting an error", oci64OERShape(),
			fixedSummaryObject(oci64OERShape(), oraInvalidNumber, 2),
		},
		{"OCI object naming no cursor", ociOERShape(), fixedSummaryObject(ociOERShape(), 0, 0)},
		{"thin object reporting success", thinOERShape(), compressedSummaryObject(1, 31, 2, 0, 9)},
		{
			"thin object reporting an error", thinOERShape(),
			compressedSummaryObject(1, 31, 2, oraInvalidNumber, 9),
		},
		{"an OCI object offered to a thin session", thinOERShape(), fixedSummaryObject(ociOERShape(), 0, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.False(t, rowEndsAtMarker(tc.shape, tc.payload, 0))
		})
	}
}

// TestRowEndsAtMarker_RefusesAnErrorObjectInsideARow is the same negative one
// level up, where it decides a row rather than a byte: a walk that drifted onto
// a summary object reporting a failure must still cost the row.
//
// It is the terminator's whole purpose expressed as behavior — the three
// column readings above it are only safe because a wrong one lands somewhere
// this refuses.
func TestRowEndsAtMarker_RefusesAnErrorObjectInsideARow(t *testing.T) {
	t.Parallel()

	const oraInvalidNumber = 1722

	shape := ociOERShape()
	types := describeColumnTypes(longColumns)

	// One row of the OCI LONG fetch, built the way the recording carries it:
	// the ROW_DATA byte, a CHAR, the LONG column with its chunked value and
	// fixed trailer, then two more CHARs.
	row := make([]byte, 0, 64)
	row = append(row, ttcMsgBindOutput, 0x06)
	row = append(row, []byte("aaaaaa")...)
	row = append(row, 0xFE, 0x03, 0x00, 0x00, 0x00, 'x', 'y', 'z', 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	row = append(row, 0x06)
	row = append(row, []byte("bbbbbb")...)
	row = append(row, 0x06)
	row = append(row, []byte("cccccc")...)

	ended := append(append([]byte(nil), row...), fixedSummaryObject(shape, 0, 2)...)
	require.Equal(t, [][]interface{}{{"aaaaaa", "xyz", "bbbbbb", "cccccc"}},
		parseRowStream(ended, 1, len(types), allColumns(len(types)), nil, types, shape, lobRowLocator),
		"a row that ends on the object sqlplus really sends must be kept")

	failed := append(append([]byte(nil), row...), fixedSummaryObject(shape, oraInvalidNumber, 2)...)
	assert.Empty(t,
		parseRowStream(failed, 1, len(types), allColumns(len(types)), nil, types, shape, lobRowLocator),
		"and one that ends on an object reporting a failure must not be")
}
