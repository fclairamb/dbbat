package oracle

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The decoder half of the OCI/sqlplus summary object, on its own rather than
// through a recording.
//
// The replay tests in midfetch_fail_replay_test.go prove it reads the bytes one
// real server sent one real client. These prove the two properties that make
// that safe to run on a path routed by a single 0x04 byte: it reads back exactly
// what encodeOERFixedWidth writes, at both widths, and it refuses everything
// that is not laid out that way — because a decoder that accepted a block of row
// data would attach a fabricated ORA error to an innocent statement, which is
// strictly worse than the missing error text it replaces.

// fixedWidthOERShape is a session that has learned its upstream marshals the
// summary object fixed-width, at the given width.
func fixedWidthOERShape(wide64 bool) oerShape {
	shape := defaultOERShape()
	shape.tailLearned = true
	shape.fixedWidth = true
	shape.fixedWidth64 = wide64

	return shape
}

// fixedWidthOERFixture builds a complete fixed-width OER through the encoder,
// so the decoder is measured against the writer rather than against a second
// hand-laid copy of the same offsets.
func fixedWidthOERFixture(wide64 bool, sum oerSummary) []byte {
	return encodeOER(fixedWidthOERShape(wide64), sum)
}

// TestDecodeOERFieldsAtLayout_ReadsBackWhatTheEncoderWrites is the round trip.
// dbbat wrote this encoding for a year without being able to read it; the two
// halves agreeing field for field, at both widths, is the whole fix.
func TestDecodeOERFieldsAtLayout_ReadsBackWhatTheEncoderWrites(t *testing.T) {
	t.Parallel()

	sum := oerSummary{
		CallStatus:   1,
		SeqNumber:    166,
		ErrorCode:    1722,
		ErrorMessage: "ORA-01722: invalid number",
		CursorID:     5,
		CallNumber:   169,
	}

	layouts := map[string]struct {
		wide64 bool
		layout oerFixedLayout
	}{
		"32-bit": {layout: oerFixed32Layout},
		"64-bit": {wide64: true, layout: oerFixed64Layout},
	}

	for name, tc := range layouts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := fixedWidthOERFixture(tc.wide64, sum)

			info, rest := decodeOERFieldsAtLayout(body, 0, tc.layout)
			require.NotNil(t, info, "the encoder's own output must decode")

			// encodeOER always sets the end-of-call bit; everything else is the
			// summary as handed in.
			assert.Equal(t, sum.CallStatus|oerEndOfCallBit, info.CallStatus)
			assert.Equal(t, sum.SeqNumber, info.SeqNumber)
			assert.Equal(t, sum.ErrorCode, info.ErrorCode)
			assert.Equal(t, sum.CursorID, info.CursorID)
			assert.Zero(t, info.CurRowNumber, "a refusal ran nothing")

			assert.LessOrEqual(t, rest, len(body), "the tail offset must stay inside the message")
			assert.Contains(t, string(body[rest:]), sum.ErrorMessage,
				"the diagnostic must be reachable from where the decode ended, which is what "+
					"lets decodeErrorOER prove the tail names the code")

			// And the whole predicate, not just the fields.
			proven := decodeErrorOER(fixedWidthOERShape(tc.wide64), body)
			require.NotNil(t, proven, "decodeErrorOER must accept it")
			assert.Equal(t, sum.ErrorMessage, proven.ErrorMessage)
		})
	}
}

// TestDecodeOERFieldsAtLayout_RefusesTheOtherLayout is why trying both widths
// under the anchor is safe rather than a coin toss: a block written for one
// layout cannot satisfy the other's invariant, so the fallback for a session
// that has learned nothing yet cannot pick the wrong one.
func TestDecodeOERFieldsAtLayout_RefusesTheOtherLayout(t *testing.T) {
	t.Parallel()

	sum := oerSummary{CallStatus: 1, ErrorCode: 942, ErrorMessage: "ORA-00942: nope", CursorID: 3}

	assert.Nil(t, firstOf(decodeOERFieldsAtLayout(fixedWidthOERFixture(false, sum), 0, oerFixed64Layout)),
		"a 32-bit block read at 64-bit offsets must not validate")
	assert.Nil(t, firstOf(decodeOERFieldsAtLayout(fixedWidthOERFixture(true, sum), 0, oerFixed32Layout)),
		"nor the other way round")

	// The unlearned fallback tries both and still lands on the right one.
	unlearned := defaultOERShape()
	require.False(t, unlearned.tailLearned)

	for _, wide64 := range []bool{false, true} {
		info, _ := decodeOERFixedFieldsAt(unlearned, fixedWidthOERFixture(wide64, sum), 0)
		require.NotNilf(t, info, "wide64=%v must decode under the unlearned fallback", wide64)
		assert.Equal(t, 942, info.ErrorCode)
		assert.Equal(t, 3, info.CursorID)
	}
}

// firstOf drops the tail offset so a decode can be asserted nil inline.
func firstOf(info *oerInfo, _ int) *oerInfo { return info }

// TestDecodeOERFieldsAtLayout_KeepsItsAnchors pins what the decoder rejects.
// Each case is a byte-level way real row data could arrive looking like a
// fixed-width summary object, and each has to fail — a false positive here
// fabricates an ORA error against a statement that succeeded.
func TestDecodeOERFieldsAtLayout_KeepsItsAnchors(t *testing.T) {
	t.Parallel()

	sound := fixedWidthOERFixture(false, oerSummary{
		CallStatus: 1, ErrorCode: 942, ErrorMessage: "ORA-00942: nope", CursorID: 3,
	})
	require.NotNil(t, firstOf(decodeOERFieldsAtLayout(sound, 0, oerFixed32Layout)),
		"the control must decode, or the cases below prove nothing")

	mutate := func(fn func([]byte)) []byte {
		out := append([]byte(nil), sound...)
		fn(out)

		return out
	}

	tests := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "the RetCode does not repeat the error number",
			payload: mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[oerFixed32Layout.retCode:], 943) }),
		},
		{
			name:    "the error number does not match the RetCode",
			payload: mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[oerFixed32Layout.errNum:], 943) }),
		},
		{
			name: "a run of zeroes satisfies the repetition and nothing else",
			payload: append([]byte{byte(TTCFuncOERR)},
				make([]byte, oerFixedWidthPrefixLen+8)...),
		},
		{
			name:    "no call status",
			payload: mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[oerFixed32Layout.callStatus:], 0) }),
		},
		{
			name:    "not a 0x04 marker",
			payload: mutate(func(b []byte) { b[0] = byte(TTCFuncResponse) }),
		},
		{name: "truncated before the RetCode", payload: sound[:oerFixed32Layout.retCode]},
		{name: "truncated before the row count", payload: sound[:oerFixedWidthPrefixLen]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Nil(t, firstOf(decodeOERFieldsAtLayout(tc.payload, 0, oerFixed32Layout)))
		})
	}
}

// TestDecodeOERFixedFieldsAt_AsksTheSessionsShape is the second half of the
// "prefer not guessing" rule: a session that has *learned* which encoding its
// upstream speaks is never offered the other one, so the fixed-width reading
// adds no surface at all to the three thin clients.
func TestDecodeOERFixedFieldsAt_AsksTheSessionsShape(t *testing.T) {
	t.Parallel()

	sum := oerSummary{CallStatus: 1, ErrorCode: 942, ErrorMessage: "ORA-00942: nope", CursorID: 3}
	body32 := fixedWidthOERFixture(false, sum)
	body64 := fixedWidthOERFixture(true, sum)

	assert.Nil(t, firstOf(decodeOERFixedFieldsAt(thinOERShape(), body32, 0)),
		"a session that learned the compressed encoding must not read a fixed-width block")
	assert.Nil(t, firstOf(decodeOERFixedFieldsAt(thinOERShape(), body64, 0)),
		"at either width")
	assert.Nil(t, decodeErrorOER(thinOERShape(), body32),
		"and the whole predicate refuses it too, which is what handleOERStatus sees")

	assert.NotNil(t, firstOf(decodeOERFixedFieldsAt(fixedWidthOERShape(false), body32, 0)))
	assert.Nil(t, firstOf(decodeOERFixedFieldsAt(fixedWidthOERShape(false), body64, 0)),
		"a learned 32-bit session is offered only its own layout")

	assert.NotNil(t, firstOf(decodeOERFixedFieldsAt(fixedWidthOERShape(true), body64, 0)))
	assert.Nil(t, firstOf(decodeOERFixedFieldsAt(fixedWidthOERShape(true), body32, 0)),
		"and a learned 64-bit one likewise")
}

// TestFindPlausibleOERInResponse_LearnsACursorFromTheBundledOCISummary is the
// cursor-learning half, against the recorded 64-bit summaries Oracle 23ai Free
// sent the client bundled in gvenzl/oracle-free:23-slim.
//
// This is what used to be missing on every OCI session, and what left the
// mid-fetch anchor with nothing to compare an OER's cursor id against: the scan
// read compressed integers only, so it walked right past the id sitting at
// oerFixed64Layout.cursorID.
func TestFindPlausibleOERInResponse_LearnsACursorFromTheBundledOCISummary(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, "testdata/oci_bundled_oer.hex")
	require.NotEmpty(t, frames)

	for i, frame := range frames {
		shape := defaultOERShape()
		require.Truef(t, learnOERShape(&shape, extractTTCPayload(frame)), "summary %d taught nothing", i)
		require.True(t, shape.fixedWidth64, "summary %d is the 64-bit OCI layout", i)

		cursorID, ok := findCursorIDInResponse(shape, frame)
		require.Truef(t, ok, "summary %d must yield a cursor id", i)
		assert.Equal(t, uint16(2), cursorID, "summary %d names cursor 2", i)

		assert.Nil(t, firstOf(decodeOERFixedFieldsAt(thinOERShape(), extractTTCPayload(frame), 0)),
			"summary %d: and a session that learned the compressed encoding still reads nothing here", i)
	}
}
