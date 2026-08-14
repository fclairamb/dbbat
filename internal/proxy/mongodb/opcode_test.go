package mongodb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// scriptedConn plays a fixed byte script at the session and records everything
// written back. net.Pipe would deadlock here: the refusal path writes its reply
// from inside the same goroutine that is reading the script.
type scriptedConn struct {
	net.Conn // never used; the four methods below are the whole surface

	in  *bytes.Reader
	out bytes.Buffer
}

func (c *scriptedConn) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *scriptedConn) Write(p []byte) (int, error) { return c.out.Write(p) }
func (*scriptedConn) Close() error                  { return nil }

// legacyFrame builds a framed message with the given opcode and an opaque body.
// The body is never decoded — that is the whole point of the refusal — so its
// only job is to be a plausible length.
func legacyFrame(opCode int32, body []byte) []byte {
	raw := make([]byte, headerLen+len(body))
	writeHeader(raw, int32(len(raw)), 7, 0, opCode)
	copy(raw[headerLen:], body)

	return raw
}

// legacyOpQueryBody is an OP_QUERY on otherdb.$cmd — the shape that used to
// reach the upstream verbatim, carrying a database the grant never covered.
func legacyOpQueryBody(t *testing.T) []byte {
	t.Helper()

	doc, err := bson.Marshal(bson.D{{Key: "find", Value: "secrets"}})
	require.NoError(t, err)

	var b bytes.Buffer

	b.Write(make([]byte, 4)) // flags
	b.WriteString("otherdb.$cmd")
	b.WriteByte(0)
	b.Write(make([]byte, 8)) // numberToSkip + numberToReturn
	b.Write(doc)

	return b.Bytes()
}

// postAuthSession is a session with no upstream: if the pump ever forwards, the
// nil upstream panics and the test fails loudly. That is the assertion.
func postAuthSession(script []byte) *scriptedConn {
	return &scriptedConn{in: bytes.NewReader(script)}
}

func newPumpSession(conn *scriptedConn) *Session {
	s := &Session{
		clientConn: conn,
		reader:     bufio.NewReader(conn),
		logger:     slog.New(slog.DiscardHandler),
		ctx:        context.Background(),
		pending:    make(map[int32]*pendingQuery),
	}
	s.replyReqID.Store(preAuthReplyRequestIDBase)

	return s
}

// A legacy opcode after authentication is refused, not relayed. Forwarding one
// verbatim skipped ValidateMongoCommand entirely — no $db check, no read_only
// check — which against a pre-5.1 upstream was a way around the grant.
func TestLegacyOpQueryRefusedPostAuth(t *testing.T) {
	t.Parallel()

	conn := postAuthSession(legacyFrame(opCodeQuery, legacyOpQueryBody(t)))
	s := newPumpSession(conn)

	// The script runs out after the one frame, so a pump that neither forwarded
	// (nil upstream → panic) nor errored returns EOF.
	require.ErrorIs(t, s.pumpClientToUpstream(), io.EOF)

	// The client gets a legacy-shaped answer it can actually read.
	reply, err := readMessage(bufio.NewReader(bytes.NewReader(conn.out.Bytes())))
	require.NoError(t, err)
	assert.Equal(t, opCodeReply, reply.opCode)
	assert.Equal(t, int32(7), reply.responseTo)

	const opReplyFixedLen = 4 + 8 + 4 + 4

	doc := bson.Raw(reply.body[opReplyFixedLen:])
	code, ok := doc.Lookup("code").Int32OK()
	require.True(t, ok)
	assert.Equal(t, int32(codeUnauthorized), code)
	assert.Contains(t, doc.Lookup("errmsg").StringValue(), "legacy wire protocol")
}

// The fire-and-forget legacy writes are dropped rather than answered — nothing
// is listening — but they are still not forwarded.
func TestLegacyWriteOpCodesDroppedNotForwarded(t *testing.T) {
	t.Parallel()

	for _, opCode := range []int32{opCodeUpdate, opCodeInsert, opCodeDelete, opCodeKillCursors} {
		conn := postAuthSession(legacyFrame(opCode, []byte{1, 2, 3, 4}))
		s := newPumpSession(conn)

		require.ErrorIs(t, s.pumpClientToUpstream(), io.EOF, opCodeName(opCode))
		assert.Empty(t, conn.out.Bytes(), "%s expects no reply", opCodeName(opCode))
	}
}

// An OP_MSG wrapped in OP_COMPRESSED still reaches the OP_MSG path; a legacy
// opcode wrapped the same way is still refused, so compression is not a way in.
func TestCompressedLegacyOpCodeStillRefused(t *testing.T) {
	t.Parallel()

	compressed, err := compressMessage(legacyFrame(opCodeGetMore, []byte{9, 9, 9, 9}), compressorZlib)
	require.NoError(t, err)

	conn := postAuthSession(compressed)
	s := newPumpSession(conn)

	require.ErrorIs(t, s.pumpClientToUpstream(), io.EOF)

	// The reply mirrors the client's compressor, so unwrap it before reading.
	outer, err := readMessage(bufio.NewReader(bytes.NewReader(conn.out.Bytes())))
	require.NoError(t, err)
	require.Equal(t, opCodeCompressed, outer.opCode)

	inner, _, err := decompressMessage(outer)
	require.NoError(t, err)
	assert.Equal(t, opCodeReply, inner.opCode)
}

// opCodeExpectsReply decides between answering and dropping; pin the split.
func TestOpCodeExpectsReply(t *testing.T) {
	t.Parallel()

	for _, opCode := range []int32{opCodeQuery, opCodeGetMore} {
		assert.True(t, opCodeExpectsReply(opCode), opCodeName(opCode))
	}

	for _, opCode := range []int32{opCodeUpdate, opCodeInsert, opCodeDelete, opCodeKillCursors} {
		assert.False(t, opCodeExpectsReply(opCode), opCodeName(opCode))
	}

	assert.Equal(t, "OP_QUERY", opCodeName(opCodeQuery))
	assert.Equal(t, "opcode 4242", opCodeName(4242))
}

// A sanity check on the frame helper, so a header mistake in the tests above
// reads as a helper bug rather than a refusal bug.
func TestLegacyFrameHeader(t *testing.T) {
	t.Parallel()

	raw := legacyFrame(opCodeQuery, []byte{1, 2, 3})
	assert.Len(t, raw, headerLen+3)
	assert.Equal(t, uint32(len(raw)), binary.LittleEndian.Uint32(raw[0:4]))
	assert.Equal(t, uint32(opCodeQuery), binary.LittleEndian.Uint32(raw[12:16]))
}
