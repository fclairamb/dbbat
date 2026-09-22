package decode

import (
	"encoding/binary"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/dump"
)

// MongoDB wire-protocol constants, mirroring internal/proxy/mongodb/wire.go.
const (
	// mongoHeaderLen is the fixed header: messageLength, requestID, responseTo
	// and opCode, four little-endian int32s.
	mongoHeaderLen = 16

	// mongoMaxMessageLen is the server's own ceiling on one message, and the
	// bound that catches a desynchronised stream before it allocates.
	mongoMaxMessageLen = 48 * 1000 * 1000

	mongoOpReply      = 1
	mongoOpQuery      = 2004
	mongoOpCompressed = 2012
	mongoOpMsg        = 2013

	// mongoFlagChecksumPresent means a 4-byte CRC-32C trails the sections.
	mongoFlagChecksumPresent = 1 << 0
	// mongoFlagMoreToCome means no reply is owed (or, on a reply, that more
	// follow unprompted).
	mongoFlagMoreToCome = 1 << 1

	mongoChecksumLen = 4
	mongoFlagsLen    = 4
	mongoMinDocLen   = 5

	// mongoOpCompressedHeaderLen is originalOpcode, uncompressedSize and the
	// compressor id.
	mongoOpCompressedHeaderLen = 9

	// mongoPendingRequestCap bounds the map of requests whose reply must stay
	// redacted, so a capture full of unanswered requests cannot grow it without
	// end.
	mongoPendingRequestCap = 1024
)

// mongoSection is one OP_MSG section: a single document (kind 0) or an
// identified sequence of them (kind 1).
type mongoSection struct {
	kind       byte
	identifier string
	documents  []bson.Raw
}

// mongoSplitter cuts both directions of a MongoDB session into messages.
//
// Framing is the easy part: every message declares its own length in its first
// four bytes. What matters here is the redaction, because a BSON document *is*
// the data — so a document is summarized by default (`find orders (filter: 2
// keys)`), exactly as a PostgreSQL DataRow collapses to its column count, and
// Options.ShowRows opts into the extended JSON.
//
// Authentication is never printed either way. That covers the SASL exchange in
// both directions: a reply is matched to the request it answers by responseTo,
// so a saslContinue's server challenge is redacted as well as the client's.
type mongoSplitter struct {
	opts Options

	clientBuf, serverBuf []byte

	// redactedRequests holds the ids of requests whose reply must stay
	// redacted — the SASL exchange, and anything else carrying a credential.
	redactedRequests map[int32]bool
}

func newMongoSplitter(opts Options) *mongoSplitter {
	return &mongoSplitter{opts: opts, redactedRequests: map[int32]bool{}}
}

// Feed consumes one packet and returns the messages it completed.
func (m *mongoSplitter) Feed(packet *dump.Packet) ([]Message, error) {
	buf := &m.clientBuf
	if packet.Direction == dump.DirServerToClient {
		buf = &m.serverBuf
	}

	*buf = append(*buf, packet.Data...)

	var out []Message

	for {
		raw, ok, err := cutMongoMessage(buf)
		if err != nil {
			return out, err
		}

		if !ok {
			return out, nil
		}

		out = append(out, Message{packet.RelativeNs, packet.Direction, m.render(packet.Direction, raw)})
	}
}

// cutMongoMessage cuts one wire message off the front of buf. ok is false when
// the message is not fully buffered yet.
func cutMongoMessage(buf *[]byte) ([]byte, bool, error) {
	if len(*buf) < mongoHeaderLen {
		return nil, false, nil
	}

	length := int(int32(binary.LittleEndian.Uint32((*buf)[:4])))
	if length < mongoHeaderLen || length > mongoMaxMessageLen {
		return nil, false, fmt.Errorf("%w: MongoDB message length %d", ErrOutOfSync, length)
	}

	if len(*buf) < length {
		return nil, false, nil
	}

	raw := (*buf)[:length]
	*buf = (*buf)[length:]

	return raw, true, nil
}

// render turns one complete wire message into a trace line.
func (m *mongoSplitter) render(direction byte, raw []byte) string {
	requestID := int32(binary.LittleEndian.Uint32(raw[4:8]))
	responseTo := int32(binary.LittleEndian.Uint32(raw[8:12]))
	opCode := int32(binary.LittleEndian.Uint32(raw[12:16]))
	body := raw[mongoHeaderLen:]

	switch opCode {
	case mongoOpMsg:
		return m.renderOpMsg(direction, requestID, responseTo, body)
	case mongoOpQuery:
		return m.renderOpQuery(requestID, body)
	case mongoOpReply:
		return m.renderOpReply(responseTo, body)
	case mongoOpCompressed:
		return renderMongoCompressed(body)
	default:
		return fmt.Sprintf("%s(%d bytes)", mongoOpCodeName(opCode), len(raw))
	}
}

// renderOpMsg is the only opcode a dbbat capture normally holds: the tap sits
// after the handshake and after decompression.
func (m *mongoSplitter) renderOpMsg(direction byte, requestID, responseTo int32, body []byte) string {
	flags, sections, ok := parseMongoOpMsg(body)
	if !ok {
		return fmt.Sprintf("OP_MSG(unreadable, %d bytes)", len(body)+mongoHeaderLen)
	}

	command, found := mongoCommandBody(sections)
	if !found {
		return "OP_MSG(no command document)" + mongoFlagSuffix(flags)
	}

	if direction == dump.DirServerToClient {
		return m.renderReplyBody(responseTo, command, sections) + mongoFlagSuffix(flags)
	}

	name := mongoCommandName(command)
	if mongoIsCredentialCommand(name, command) {
		m.markRedacted(requestID)

		return name + " (redacted)" + mongoFlagSuffix(flags)
	}

	return formatMongoCommand(name, command, sections, m.opts) + mongoFlagSuffix(flags)
}

// renderReplyBody renders the server's answer, keeping it redacted when the
// request it answers was.
func (m *mongoSplitter) renderReplyBody(responseTo int32, command bson.Raw, sections []mongoSection) string {
	if m.redactedRequests[responseTo] {
		delete(m.redactedRequests, responseTo)

		return "Reply (redacted)"
	}

	return formatMongoReply(command, sections, m.opts)
}

// renderOpQuery reads the legacy opcode a driver still uses for its very first
// handshake. Post-handshake, dbbat refuses legacy opcodes outright.
func (m *mongoSplitter) renderOpQuery(requestID int32, body []byte) string {
	collection, query, ok := parseMongoOpQuery(body)
	if !ok {
		return fmt.Sprintf("OP_QUERY(unreadable, %d bytes)", len(body)+mongoHeaderLen)
	}

	name := mongoCommandName(query)
	if mongoIsCredentialCommand(name, query) {
		m.markRedacted(requestID)

		return "OP_QUERY " + collection + " " + name + " (redacted)"
	}

	return "OP_QUERY " + collection + " " + formatMongoCommand(name, query, nil, m.opts)
}

// renderOpReply reads the legacy reply that answers an OP_QUERY handshake.
func (m *mongoSplitter) renderOpReply(responseTo int32, body []byte) string {
	const prefix = 20 // responseFlags, cursorID, startingFrom, numberReturned

	if len(body) < prefix {
		return fmt.Sprintf("OP_REPLY(unreadable, %d bytes)", len(body)+mongoHeaderLen)
	}

	if m.redactedRequests[responseTo] {
		delete(m.redactedRequests, responseTo)

		return "OP_REPLY (redacted)"
	}

	doc, _, ok := readMongoDoc(body[prefix:])
	if !ok {
		return "OP_REPLY(no document)"
	}

	return "OP_REPLY " + formatMongoReply(doc, nil, m.opts)
}

// markRedacted remembers that this request's reply must not be printed either.
func (m *mongoSplitter) markRedacted(requestID int32) {
	if len(m.redactedRequests) >= mongoPendingRequestCap {
		clear(m.redactedRequests)
	}

	m.redactedRequests[requestID] = true
}

// parseMongoOpMsg splits an OP_MSG body into its flag bits and its sections.
func parseMongoOpMsg(body []byte) (uint32, []mongoSection, bool) {
	if len(body) < mongoFlagsLen {
		return 0, nil, false
	}

	flags := binary.LittleEndian.Uint32(body[:mongoFlagsLen])
	rest := body[mongoFlagsLen:]

	if flags&mongoFlagChecksumPresent != 0 {
		if len(rest) < mongoChecksumLen {
			return flags, nil, false
		}

		rest = rest[:len(rest)-mongoChecksumLen]
	}

	var sections []mongoSection

	for len(rest) > 0 {
		section, used, ok := parseMongoSection(rest)
		if !ok {
			return flags, sections, false
		}

		sections = append(sections, section)
		rest = rest[used:]
	}

	return flags, sections, true
}

// parseMongoSection reads one section: a bare document, or a length-prefixed
// run of them under an identifier.
func parseMongoSection(b []byte) (mongoSection, int, bool) {
	if len(b) == 0 {
		return mongoSection{}, 0, false
	}

	switch b[0] {
	case 0:
		doc, used, ok := readMongoDoc(b[1:])
		if !ok {
			return mongoSection{}, 0, false
		}

		return mongoSection{kind: 0, documents: []bson.Raw{doc}}, used + 1, true
	case 1:
		return parseMongoSequence(b)
	default:
		return mongoSection{}, 0, false
	}
}

// parseMongoSequence reads a kind-1 section: an int32 size counting itself, a
// NUL-terminated identifier, then documents until the size is used up.
func parseMongoSequence(b []byte) (mongoSection, int, bool) {
	const sizeLen = 4

	if len(b) < 1+sizeLen {
		return mongoSection{}, 0, false
	}

	size := int(int32(binary.LittleEndian.Uint32(b[1 : 1+sizeLen])))
	if size < sizeLen || 1+size > len(b) {
		return mongoSection{}, 0, false
	}

	payload := b[1+sizeLen : 1+size]

	identifier, used, ok := cstring(payload)
	if !ok {
		return mongoSection{}, 0, false
	}

	section := mongoSection{kind: 1, identifier: identifier}
	rest := payload[used:]

	for len(rest) > 0 {
		doc, docUsed, ok := readMongoDoc(rest)
		if !ok {
			return mongoSection{}, 0, false
		}

		section.documents = append(section.documents, doc)
		rest = rest[docUsed:]
	}

	return section, 1 + size, true
}

// parseMongoOpQuery reads the legacy query: flags, the collection name, two
// int32 counts, then the query document.
func parseMongoOpQuery(body []byte) (string, bson.Raw, bool) {
	const flagsLen = 4

	const skipReturnLen = 8

	if len(body) < flagsLen {
		return "", nil, false
	}

	collection, used, ok := cstring(body[flagsLen:])
	if !ok {
		return "", nil, false
	}

	rest := body[flagsLen+used:]
	if len(rest) < skipReturnLen {
		return "", nil, false
	}

	doc, _, ok := readMongoDoc(rest[skipReturnLen:])
	if !ok {
		return "", nil, false
	}

	return collection, doc, true
}

// readMongoDoc cuts one BSON document, which declares its own length.
func readMongoDoc(b []byte) (bson.Raw, int, bool) {
	if len(b) < mongoMinDocLen {
		return nil, 0, false
	}

	size := int(int32(binary.LittleEndian.Uint32(b[:4])))
	if size < mongoMinDocLen || size > len(b) {
		return nil, 0, false
	}

	return bson.Raw(b[:size]), size, true
}

// renderMongoCompressed names a compressed envelope without inflating it.
// dbbat's tap records the inflated message, so this only appears on a capture
// made some other way.
func renderMongoCompressed(body []byte) string {
	if len(body) < mongoOpCompressedHeaderLen {
		return fmt.Sprintf("OP_COMPRESSED(unreadable, %d bytes)", len(body)+mongoHeaderLen)
	}

	original := int32(binary.LittleEndian.Uint32(body[:4]))
	size := int32(binary.LittleEndian.Uint32(body[4:8]))

	return fmt.Sprintf("OP_COMPRESSED(%s, %d bytes uncompressed, compressor %d)",
		mongoOpCodeName(original), size, body[8])
}
