package mongodb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOpMsgReplyRoundTrip(t *testing.T) {
	t.Parallel()

	doc := bson.D{{Key: "ok", Value: 1.0}, {Key: "hello", Value: "world"}}

	raw, err := buildOpMsgReply(7, 42, doc)
	require.NoError(t, err)

	m, err := readMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, opCodeMsg, m.opCode)
	assert.Equal(t, int32(7), m.requestID)
	assert.Equal(t, int32(42), m.responseTo)

	parsed, err := parseOpMsg(m.body)
	require.NoError(t, err)

	body, ok := parsed.commandBody()
	require.True(t, ok)
	assert.Equal(t, "ok", commandName(body))
	assert.Equal(t, "world", lookupString(body, "hello"))
}

func TestOpReplyRoundTrip(t *testing.T) {
	t.Parallel()

	doc := bson.D{{Key: "ismaster", Value: true}, {Key: "ok", Value: 1.0}}

	raw, err := buildOpReply(3, 99, doc)
	require.NoError(t, err)

	m, err := readMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, opCodeReply, m.opCode)
	assert.Equal(t, int32(99), m.responseTo)
}

// TestParseOpMsgWithChecksum verifies a trailing CRC-32C checksum is tolerated
// (stripped) on parse (contract §1).
func TestParseOpMsgWithChecksum(t *testing.T) {
	t.Parallel()

	docBytes, err := bson.Marshal(bson.D{{Key: "ping", Value: 1}})
	require.NoError(t, err)

	var buf bytes.Buffer

	var flags [4]byte
	binary.LittleEndian.PutUint32(flags[:], flagChecksumPresent)
	buf.Write(flags[:])
	buf.WriteByte(0) // section kind 0
	buf.Write(docBytes)
	buf.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF}) // fake CRC-32C

	parsed, err := parseOpMsg(buf.Bytes())
	require.NoError(t, err)

	body, ok := parsed.commandBody()
	require.True(t, ok)
	assert.Equal(t, "ping", commandName(body))
}

// TestParseOpMsgKind1Sequence verifies a kind-1 document sequence (e.g.
// insert's "documents") parses alongside the kind-0 body.
func TestParseOpMsgKind1Sequence(t *testing.T) {
	t.Parallel()

	cmdBytes, err := bson.Marshal(bson.D{{Key: "insert", Value: "coll"}, {Key: "$db", Value: "app"}})
	require.NoError(t, err)

	doc1, err := bson.Marshal(bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)
	doc2, err := bson.Marshal(bson.D{{Key: "_id", Value: 2}})
	require.NoError(t, err)

	identifier := "documents"
	seqBody := append([]byte(identifier), 0)
	seqBody = append(seqBody, doc1...)
	seqBody = append(seqBody, doc2...)

	seqSize := 4 + len(seqBody)
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], uint32(seqSize))

	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0}) // flags
	buf.WriteByte(0)              // kind 0
	buf.Write(cmdBytes)
	buf.WriteByte(1) // kind 1
	buf.Write(sizeBuf[:])
	buf.Write(seqBody)

	parsed, err := parseOpMsg(buf.Bytes())
	require.NoError(t, err)

	body, ok := parsed.commandBody()
	require.True(t, ok)
	assert.Equal(t, "insert", commandName(body))
	assert.Equal(t, "app", lookupString(body, "$db"))

	docs, ok := parsed.sequence("documents")
	require.True(t, ok)
	require.Len(t, docs, 2)
}

func TestParseOpQueryHello(t *testing.T) {
	t.Parallel()

	queryBytes, err := bson.Marshal(bson.D{{Key: "isMaster", Value: 1}, {Key: "helloOk", Value: true}})
	require.NoError(t, err)

	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0}) // flags
	buf.WriteString("admin.$cmd")
	buf.WriteByte(0)                          // cstring terminator
	buf.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0}) // numberToSkip + numberToReturn
	buf.Write(queryBytes)

	q, err := parseOpQuery(buf.Bytes())
	require.NoError(t, err)
	assert.Equal(t, "admin.$cmd", q.fullCollectionName)
	assert.Equal(t, "isMaster", commandName(q.query))
	assert.True(t, lookupBool(q.query, "helloOk"))
}

// TestCompressDecompressRoundTrip verifies an OP_MSG frame survives a
// compress → decompress round-trip for both supported compressors, restoring
// the original opcode, ids and body (item 4).
func TestCompressDecompressRoundTrip(t *testing.T) {
	t.Parallel()

	for _, compressorID := range []uint8{compressorNoop, compressorZlib} {
		original, err := buildOpMsgReply(11, 22, bson.D{{Key: "find", Value: "widgets"}, {Key: "$db", Value: "app"}})
		require.NoError(t, err)

		wrapped, err := compressMessage(original, compressorID)
		require.NoError(t, err)

		wm, err := readMessage(bytes.NewReader(wrapped))
		require.NoError(t, err)
		assert.Equal(t, opCodeCompressed, wm.opCode)

		inner, gotID, err := decompressMessage(wm, maxWireMessageSize)
		require.NoError(t, err)
		assert.Equal(t, compressorID, gotID)
		assert.Equal(t, opCodeMsg, inner.opCode)
		assert.Equal(t, int32(11), inner.requestID)
		assert.Equal(t, int32(22), inner.responseTo)
		assert.Equal(t, original, inner.raw, "round-trip must restore the exact frame")

		body, ok := parseOpMsgBody(t, inner)
		require.True(t, ok)
		assert.Equal(t, "find", commandName(body))
	}
}

// TestDecompressRejectsUnsupportedCompressor verifies snappy/zstd frames are
// rejected (dbbat never negotiates them).
func TestDecompressRejectsUnsupportedCompressor(t *testing.T) {
	t.Parallel()

	original, err := buildOpMsgReply(1, 2, bson.D{{Key: "ping", Value: 1}})
	require.NoError(t, err)

	// Hand-craft an OP_COMPRESSED claiming compressor id 1 (snappy).
	wrapped, err := compressMessage(original, compressorNoop)
	require.NoError(t, err)
	wrapped[24] = compressorSnappy

	wm, err := readMessage(bytes.NewReader(wrapped))
	require.NoError(t, err)

	_, _, err = decompressMessage(wm, maxWireMessageSize)
	require.ErrorIs(t, err, ErrUnsupportedCompress)
}

func parseOpMsgBody(t *testing.T, m *message) (bson.Raw, bool) {
	t.Helper()

	parsed, err := parseOpMsg(m.body)
	require.NoError(t, err)

	return parsed.commandBody()
}

func TestReadMessageRejectsShortLength(t *testing.T) {
	t.Parallel()

	var buf [16]byte
	binary.LittleEndian.PutUint32(buf[0:4], 4) // length < headerLen

	_, err := readMessage(bytes.NewReader(buf[:]))
	require.ErrorIs(t, err, ErrShortMessage)
}

// compressedFrame wraps doc in an OP_MSG, then in an OP_COMPRESSED envelope,
// and, when declared >= 0, overwrites the envelope's uncompressedSize with it.
func compressedFrame(t *testing.T, compressorID uint8, doc any, declared int) []byte {
	t.Helper()

	original, err := buildOpMsgReply(5, 0, doc)
	require.NoError(t, err)

	wrapped, err := compressMessage(original, compressorID)
	require.NoError(t, err)

	if declared >= 0 {
		binary.LittleEndian.PutUint32(wrapped[20:24], uint32(declared))
	}

	return wrapped
}

// sessionReading is a session whose client stream is raw, in the given
// authentication state — enough for readClientMessage.
func sessionReading(raw []byte, authenticated bool) *Session {
	conn := &scriptedConn{in: bytes.NewReader(raw)}

	return &Session{
		clientConn:    conn,
		reader:        bufio.NewReader(conn),
		logger:        slog.New(slog.DiscardHandler),
		ctx:           context.Background(),
		authenticated: authenticated,
	}
}

// Before authentication an OP_COMPRESSED frame declaring more than the
// pre-auth cap is refused on the header alone: nothing near the declared size
// is allocated. This used to inflate (and then copy) up to 48 MB per frame.
//
//nolint:paralleltest // measures process-wide allocation; a parallel test would pollute it
func TestDecompressMessage_PreAuthRefusesOversizedDeclaration(t *testing.T) {

	const declared = 40 * 1000 * 1000

	frame := compressedFrame(t, compressorZlib, bson.D{{Key: "hello", Value: 1}}, declared)

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	_, err := sessionReading(frame, false).readClientMessage()

	runtime.ReadMemStats(&after)

	require.ErrorIs(t, err, ErrMessageTooLarge)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(declared/10),
		"the declared size must not be allocated before it is refused")
}

// A frame that lies about its size — declares something small, inflates to
// much more — stops at the declaration instead of inflating it all.
//
//nolint:paralleltest // measures process-wide allocation; a parallel test would pollute it
func TestDecompressMessage_InflationStopsAtDeclaredSize(t *testing.T) {

	big := bytes.Repeat([]byte{'a'}, 8*1024*1024)
	frame := compressedFrame(t, compressorZlib, bson.D{{Key: "insert", Value: "c"}, {Key: "pad", Value: big}}, 1024)

	wm, err := readMessage(bytes.NewReader(frame))
	require.NoError(t, err)

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	_, _, err = decompressMessage(wm, maxWireMessageSize)

	runtime.ReadMemStats(&after)

	require.ErrorIs(t, err, ErrCompressedSizeMismat)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(len(big)/4),
		"inflation must be bounded by the declared size, not by the payload")
}

// Positive control: a small compressed hello still works before
// authentication, under either compressor.
func TestDecompressMessage_PreAuthSmallHelloWorks(t *testing.T) {
	t.Parallel()

	for _, compressorID := range []uint8{compressorNoop, compressorZlib} {
		frame := compressedFrame(t, compressorID, bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}, -1)

		s := sessionReading(frame, false)

		m, err := s.readClientMessage()
		require.NoError(t, err)
		assert.Equal(t, opCodeMsg, m.opCode)
		assert.True(t, s.compressReplies)

		body, ok := parseOpMsgBody(t, m)
		require.True(t, ok)
		assert.Equal(t, "hello", commandName(body))
	}
}

// After authentication the full wire limit applies: a compressed command well
// past the pre-auth cap is inflated, while the same frame is refused before.
func TestDecompressMessage_PostAuthKeepsWireLimit(t *testing.T) {
	t.Parallel()

	pad := bytes.Repeat([]byte{'b'}, 4*preAuthMaxUncompressedSize)
	doc := bson.D{{Key: "insert", Value: "c"}, {Key: "pad", Value: pad}, {Key: "$db", Value: "app"}}
	frame := compressedFrame(t, compressorZlib, doc, -1)

	_, err := sessionReading(frame, false).readClientMessage()
	require.ErrorIs(t, err, ErrMessageTooLarge)

	m, err := sessionReading(frame, true).readClientMessage()
	require.NoError(t, err)

	body, ok := parseOpMsgBody(t, m)
	require.True(t, ok)
	assert.Equal(t, "insert", commandName(body))

	wm, err := readMessage(bytes.NewReader(compressedFrame(t, compressorZlib, doc, maxWireMessageSize+1)))
	require.NoError(t, err)

	_, _, err = decompressMessage(wm, maxWireMessageSize)
	require.ErrorIs(t, err, ErrMessageTooLarge)
}
