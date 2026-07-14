package mongodb

import (
	"bytes"
	"encoding/binary"
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

		inner, gotID, err := decompressMessage(wm)
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

	_, _, err = decompressMessage(wm)
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
