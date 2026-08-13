package oracle

import (
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// bundledOCIFirstCall is the first message the DB-bundled OCI client (sqlplus
// 23.26, from gvenzl/oracle-free:23-slim) sends once AUTH completes, recorded
// off the wire against a live proxy. It is the TNS Data packet payload: two
// data-flag bytes, then the TTC message.
//
// It is here rather than in a pcapng because it is one packet and the point is
// the bytes themselves — the unit half of this regression needs no Oracle
// container, which is what made the fix cheap to iterate on.
func bundledOCIFirstCall(t *testing.T) []byte {
	t.Helper()

	return recordedFrames(t, "testdata/oci_bundled_first_call.hex")[0]
}

// bundledOCICloseCursors is the same client's close-cursors piggybacks, in the
// 64-bit header the wide walk had to learn.
func bundledOCICloseCursors(t *testing.T) [][]byte {
	t.Helper()

	return recordedFrames(t, "testdata/oci_bundled_close_cursors.hex")
}

// recordedFrames reads a hex recording: one TNS Data payload per non-comment
// line, `#` for the commentary that says where the bytes came from.
func recordedFrames(t *testing.T, path string) [][]byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var frames [][]byte

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		frame, err := hex.DecodeString(line)
		require.NoError(t, err)
		frames = append(frames, frame)
	}

	require.NotEmpty(t, frames, "%s carries no frames", path)

	return frames
}

// TestBundledOCIFirstCallIsTheFrameThatWasMisread guards the fixture, so every
// assertion below is about the real defect rather than about some other bytes.
// It asserts the two readings that made this a bug: dbbat calls the message a
// fetch, and the fetch decode pulls 27396 out of it — the exact cursor id in
// the WARN this spec was filed from.
func TestBundledOCIFirstCallIsTheFrameThatWasMisread(t *testing.T) {
	t.Parallel()

	payload := bundledOCIFirstCall(t)

	funcCode, err := parseTTCFunctionCode(payload)
	require.NoError(t, err)
	assert.Equal(t, TTCFuncOFETCH, funcCode, "the message type dbbat still calls OFETCH")

	ttc := extractTTCPayload(payload)
	require.NotNil(t, ttc)
	assert.Equal(t, "0x6b", ttcOpFunction(ttc), "the TTC function this piggyback really carries")

	decoded, err := decodeOFETCH(ttc)
	require.NoError(t, err)
	assert.Equal(t, uint16(27396), decoded.CursorID,
		"the fetch layout reads (function, sequence) as a cursor id — this is the misread")

	assert.False(t, IsCloseCursorsPiggyback(ttc), "and it is not the one 0x11 frame dbbat can walk")
	assert.False(t, IsExecSQL(ttc), "nor an exec it can identify from its contents")
}

// TestClientCallNumberDeclinesAnUnwalkablePiggyback is the hang's own half.
//
// The client is parked on the call stapled *behind* this piggyback (`03 3b`,
// sequence 5), which sits past a body dbbat cannot walk. Naming the piggyback's
// own sequence (4) would make a refusal end a call nobody is waiting for, and
// the client then waits for the real one forever — measured: sqlplus never came
// back and never printed a line, not even the output of the statement before
// the refused one.
func TestClientCallNumberDeclinesAnUnwalkablePiggyback(t *testing.T) {
	t.Parallel()

	ttc := extractTTCPayload(bundledOCIFirstCall(t))

	assert.Equal(t, byte(0x04), ttc[ttcOpSeqOffset],
		"the piggyback's own sequence is right there, which is what made this tempting")
	assert.Contains(t, string(ttc), "\x03\x3b\x05",
		"and the call the client is parked on is stapled behind it, one higher")

	_, ok := clientCallNumber(ttc)
	assert.False(t, ok, "dbbat must decline to name a call it cannot see")
}

// TestBundledOCICloseCursorsWalkFindsTheStapledCall is the other half of the
// hang, and the half fail-open could not have covered: the frame it is about
// carries an INSERT, which a `read_only` grant has to refuse. dbbat cannot get
// out of the way of that one — it has to answer it, and answer the right call.
//
// The call is stapled behind a close-cursors list written in the 64-bit OCI
// header, which the wide walk did not recognize: the list did not decode, the
// staple was never found, and the refusal went out carrying whatever sequence
// number dbbat had last seen. sqlplus waited for the answer to sequence 14
// forever.
func TestBundledOCICloseCursorsWalkFindsTheStapledCall(t *testing.T) {
	t.Parallel()

	frames := bundledOCICloseCursors(t)
	require.Len(t, frames, 2)

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotNil(t, ttc)

		require.True(t, IsCloseCursorsPiggyback(ttc))
		assert.False(t, isCloseCursorsWideHeader(ttc),
			"frame %d is not the 4-byte OCI header — that is the whole point", i)
		assert.True(t, isCloseCursorsWide8Header(ttc), "frame %d must be read as the 64-bit header", i)

		ids, err := decodeCloseCursors(ttc)
		require.NoErrorf(t, err, "frame %d", i)
		assert.Equal(t, []uint16{2}, ids, "frame %d closes the one cursor it names", i)
	}

	// The second frame is the INSERT's, and 14 is the sequence its stapled
	// `03 5e` carries. Anything else strands the client.
	call, ok := clientCallNumber(extractTTCPayload(frames[1]))
	require.True(t, ok)
	assert.Equal(t, byte(0x0e), call,
		"the refusal must end the call stapled behind the close list, not the piggyback")
}

// TestObserveClientCallNumberKeepsTheLastNamedCall pins the second-order half:
// an unnameable message must not overwrite the last call dbbat did name. The
// response leg's mid-stream limit refusal reads that number while the client is
// parked on a call, so poisoning it with a piggyback's sequence would strand an
// OCI client on a path that has nothing to do with cursors.
func TestObserveClientCallNumberKeepsTheLastNamedCall(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

	require.True(t, s.observeClientCallNumber([]byte{0x03, 0x5e, 0x09, 0x00}))
	require.Equal(t, byte(0x09), s.oerCallNumber)

	assert.False(t, s.observeClientCallNumber(extractTTCPayload(bundledOCIFirstCall(t))))
	assert.Equal(t, byte(0x09), s.oerCallNumber,
		"a call dbbat could not name must leave the last one it could alone")
}

// TestBundledOCIFirstCallIsForwardedUnderARestrictiveGrant is the regression
// itself, and it is deliberately run under the grant that broke: `read_only`
// makes hasStatementControls true, which is what turned the misread into a
// refusal rather than a WARN.
//
// Two assertions, one per symptom. The frame travels upstream (it is session
// state the client needs, not an execution), and dbbat writes nothing back — a
// refusal here is an OER for a call the client is not parked on, which is the
// hang.
func TestBundledOCIFirstCallIsForwardedUnderARestrictiveGrant(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.logger = slog.New(logs)

	client, answered := recordingPipe(t)
	s.clientConn = client

	pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: bundledOCIFirstCall(t)}

	assert.False(t, s.interceptClientMessage(pkt),
		"the bundled OCI client's first message must reach its upstream")

	assert.Zero(t, logs.count(logMsgUntrackedCursorRefused),
		"and must not be refused as a re-execution of a cursor nobody opened")
	assert.Equal(t, 1, logs.count(logMsgUnnamedCallForwarded),
		"the fail-open must be recorded, or a future misread would be silent again")

	assert.Empty(t, answered(), "nothing may be written back: the client is parked on another call")
}
