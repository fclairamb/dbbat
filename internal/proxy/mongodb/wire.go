// Package mongodb implements a transparent MongoDB wire-protocol proxy for
// dbbat: it terminates client authentication (SASL PLAIN over TLS or a dbb_
// API key), authenticates to the upstream MongoDB with stored credentials
// (SCRAM-SHA-256), and grant-checks, classifies, logs and quota-enforces
// every command — the same pipeline as the PostgreSQL/Oracle/MySQL proxies.
//
// The wire framing is hand-rolled (no Go library offers a MongoDB *server*
// handshake) following the contract in
// specs/todos/2026-07-14-mongodb-support.md §1–§7. BSON document
// encode/decode uses go.mongodb.org/mongo-driver/v2/bson.
package mongodb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Wire opcodes (contract §1).
const (
	opCodeReply      int32 = 1    // legacy server reply to OP_QUERY
	opCodeQuery      int32 = 2004 // legacy client request (first hello)
	opCodeCompressed int32 = 2012 // OP_COMPRESSED: zlib negotiated in hello
	opCodeMsg        int32 = 2013 // everything, MongoDB >= 3.6
)

// Wire compressor IDs (OP_COMPRESSED). dbbat negotiates only zlib (stdlib); a
// noop-compressed frame is also accepted. snappy (1) and zstd (3) are declined
// during hello negotiation, so a client never sends them.
const (
	compressorNoop   uint8 = 0
	compressorSnappy uint8 = 1
	compressorZlib   uint8 = 2
	compressorZstd   uint8 = 3
)

// opCompressedHeaderLen is the fixed prefix of an OP_COMPRESSED body:
// originalOpcode(4) + uncompressedSize(4) + compressorId(1).
const opCompressedHeaderLen = 9

// headerLen is the fixed 16-byte message header size.
const headerLen = 16

// maxWireMessageSize bounds an inbound message so a malformed or hostile
// length prefix can't make us allocate unbounded memory. Set slightly above
// the maxMessageSizeBytes we advertise in hello (48 MB).
const maxWireMessageSize = 48 * 1000 * 1000

// OP_MSG flag bits (contract §1).
const (
	flagChecksumPresent uint32 = 1 << 0  // 4-byte CRC-32C trails the message
	flagMoreToCome      uint32 = 1 << 1  // sender expects no reply (w:0 writes)
	flagExhaustAllowed  uint32 = 1 << 16 // client permits streamed replies (ignored)
)

// opReplyAwaitCapable is the OP_REPLY responseFlags bit we set (AwaitCapable).
const opReplyAwaitCapable uint32 = 8

// Wire-parsing errors.
var (
	ErrShortMessage         = errors.New("mongodb: message shorter than declared")
	ErrMessageTooLarge      = errors.New("mongodb: message exceeds maximum size")
	ErrUnsupportedCompress  = errors.New("mongodb: unsupported OP_COMPRESSED compressor")
	ErrCompressedSizeMismat = errors.New("mongodb: OP_COMPRESSED uncompressed size mismatch")
	ErrUnknownOpCode        = errors.New("mongodb: unknown opcode")
	ErrBadSection           = errors.New("mongodb: invalid OP_MSG section kind")
	ErrNoCommandBody        = errors.New("mongodb: OP_MSG has no kind-0 command body")
	ErrEmptyCommandBody     = errors.New("mongodb: empty command document")
)

// message is a raw wire-protocol message: the parsed 16-byte header plus the
// full byte slice (raw) for verbatim relay (contract §6).
type message struct {
	length     int32
	requestID  int32
	responseTo int32
	opCode     int32
	body       []byte // bytes after the 16-byte header
	raw        []byte // full message including header (for verbatim relay)
}

// readMessage reads one framed message from r. It reads the 16-byte header,
// then messageLength-16 body bytes.
func readMessage(r io.Reader) (*message, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}

	length := int32(binary.LittleEndian.Uint32(hdr[0:4]))
	if length < headerLen {
		return nil, fmt.Errorf("%w: length %d", ErrShortMessage, length)
	}

	if int(length) > maxWireMessageSize {
		return nil, fmt.Errorf("%w: %d", ErrMessageTooLarge, length)
	}

	raw := make([]byte, length)
	copy(raw[:headerLen], hdr[:])

	if _, err := io.ReadFull(r, raw[headerLen:]); err != nil {
		return nil, err
	}

	return &message{
		length:     length,
		requestID:  int32(binary.LittleEndian.Uint32(hdr[4:8])),
		responseTo: int32(binary.LittleEndian.Uint32(hdr[8:12])),
		opCode:     int32(binary.LittleEndian.Uint32(hdr[12:16])),
		body:       raw[headerLen:],
		raw:        raw,
	}, nil
}

// decompressMessage unwraps an OP_COMPRESSED message (opcode 2012) into the
// logical message it carries. The body layout is originalOpcode(int32) +
// uncompressedSize(int32) + compressorId(uint8) + compressed bytes. The
// returned message's raw is the reconstructed *uncompressed* frame, ready for
// verbatim relay upstream (which never negotiates compression). The compressor
// id actually used is returned so replies can mirror it.
func decompressMessage(m *message) (*message, uint8, error) {
	if len(m.body) < opCompressedHeaderLen {
		return nil, 0, ErrShortMessage
	}

	originalOpcode := int32(binary.LittleEndian.Uint32(m.body[0:4]))
	uncompressedSize := int(binary.LittleEndian.Uint32(m.body[4:8]))
	compressorID := m.body[8]
	compressed := m.body[opCompressedHeaderLen:]

	if uncompressedSize < 0 || uncompressedSize > maxWireMessageSize {
		return nil, 0, fmt.Errorf("%w: %d", ErrMessageTooLarge, uncompressedSize)
	}

	innerBody, err := decompress(compressorID, compressed, uncompressedSize)
	if err != nil {
		return nil, 0, err
	}

	if len(innerBody) != uncompressedSize {
		return nil, 0, ErrCompressedSizeMismat
	}

	total := headerLen + len(innerBody)

	raw := make([]byte, total)
	writeHeader(raw, int32(total), m.requestID, m.responseTo, originalOpcode)
	copy(raw[headerLen:], innerBody)

	return &message{
		length:     int32(total),
		requestID:  m.requestID,
		responseTo: m.responseTo,
		opCode:     originalOpcode,
		body:       raw[headerLen:],
		raw:        raw,
	}, compressorID, nil
}

// compressMessage wraps a full uncompressed frame (with its 16-byte header) in
// an OP_COMPRESSED envelope using the given compressor, preserving requestID /
// responseTo and recording the original opcode.
func compressMessage(raw []byte, compressorID uint8) ([]byte, error) {
	if len(raw) < headerLen {
		return nil, ErrShortMessage
	}

	requestID := int32(binary.LittleEndian.Uint32(raw[4:8]))
	responseTo := int32(binary.LittleEndian.Uint32(raw[8:12]))
	originalOpcode := int32(binary.LittleEndian.Uint32(raw[12:16]))
	innerBody := raw[headerLen:]

	compressed, err := compress(compressorID, innerBody)
	if err != nil {
		return nil, err
	}

	total := headerLen + opCompressedHeaderLen + len(compressed)

	buf := make([]byte, total)
	writeHeader(buf, int32(total), requestID, responseTo, opCodeCompressed)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(originalOpcode))
	binary.LittleEndian.PutUint32(buf[20:24], uint32(len(innerBody)))
	buf[24] = compressorID
	copy(buf[25:], compressed)

	return buf, nil
}

// decompress inflates data per the compressor id. Only noop and zlib are
// supported (the only compressors dbbat negotiates).
func decompress(compressorID uint8, data []byte, uncompressedSize int) ([]byte, error) {
	switch compressorID {
	case compressorNoop:
		out := make([]byte, len(data))
		copy(out, data)

		return out, nil
	case compressorZlib:
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("mongodb: zlib reader: %w", err)
		}
		defer func() { _ = zr.Close() }()

		// Bound inflation to the declared size + 1 so a malicious frame can't
		// expand unbounded; the caller rejects any length != uncompressedSize.
		out, err := io.ReadAll(io.LimitReader(zr, int64(uncompressedSize)+1))
		if err != nil {
			return nil, fmt.Errorf("mongodb: zlib inflate: %w", err)
		}

		return out, nil
	default:
		return nil, fmt.Errorf("%w: id %d", ErrUnsupportedCompress, compressorID)
	}
}

// compress deflates data per the compressor id (noop or zlib).
func compress(compressorID uint8, data []byte) ([]byte, error) {
	switch compressorID {
	case compressorNoop:
		out := make([]byte, len(data))
		copy(out, data)

		return out, nil
	case compressorZlib:
		var buf bytes.Buffer

		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(data); err != nil {
			_ = zw.Close()

			return nil, fmt.Errorf("mongodb: zlib deflate: %w", err)
		}

		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("mongodb: zlib close: %w", err)
		}

		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("%w: id %d", ErrUnsupportedCompress, compressorID)
	}
}

// compressorIDForName maps a hello-advertised compressor name to its wire id,
// returning (0,false) for names dbbat doesn't support.
func compressorIDForName(name string) (uint8, bool) {
	switch name {
	case "zlib":
		return compressorZlib, true
	default:
		return 0, false
	}
}

// opMsgSection is one OP_MSG section: a single document (kind 0) or a named
// document sequence (kind 1).
type opMsgSection struct {
	kind       byte
	identifier string     // kind 1 only
	documents  []bson.Raw // kind 0 → exactly one; kind 1 → zero or more
}

// opMsg is a parsed OP_MSG payload.
type opMsg struct {
	flags    uint32
	sections []opMsgSection
}

// parseOpMsg parses an OP_MSG body (contract §1). A trailing CRC-32C checksum
// is tolerated (stripped and ignored) when the checksumPresent flag is set.
func parseOpMsg(body []byte) (*opMsg, error) {
	if len(body) < 4 {
		return nil, ErrShortMessage
	}

	flags := binary.LittleEndian.Uint32(body[0:4])
	rest := body[4:]

	if flags&flagChecksumPresent != 0 {
		if len(rest) < 4 {
			return nil, ErrShortMessage
		}

		rest = rest[:len(rest)-4]
	}

	msg := &opMsg{flags: flags}

	for len(rest) > 0 {
		kind := rest[0]
		rest = rest[1:]

		switch kind {
		case 0:
			doc, err := readBSONDoc(rest)
			if err != nil {
				return nil, err
			}

			msg.sections = append(msg.sections, opMsgSection{kind: 0, documents: []bson.Raw{doc}})
			rest = rest[len(doc):]

		case 1:
			section, consumed, err := parseDocumentSequence(rest)
			if err != nil {
				return nil, err
			}

			msg.sections = append(msg.sections, section)
			rest = rest[consumed:]

		default:
			return nil, fmt.Errorf("%w: %d", ErrBadSection, kind)
		}
	}

	return msg, nil
}

// parseDocumentSequence parses a kind-1 section body (the bytes after the kind
// byte): int32 size (inclusive), cstring identifier, then consecutive docs.
func parseDocumentSequence(rest []byte) (opMsgSection, int, error) {
	if len(rest) < 4 {
		return opMsgSection{}, 0, ErrShortMessage
	}

	size := int(binary.LittleEndian.Uint32(rest[0:4]))
	if size < 5 || size > len(rest) {
		return opMsgSection{}, 0, fmt.Errorf("%w: sequence size %d", ErrShortMessage, size)
	}

	inner := rest[4:size]

	idEnd := indexByte(inner, 0)
	if idEnd < 0 {
		return opMsgSection{}, 0, ErrShortMessage
	}

	identifier := string(inner[:idEnd])
	docsBytes := inner[idEnd+1:]

	var docs []bson.Raw

	for len(docsBytes) > 0 {
		doc, err := readBSONDoc(docsBytes)
		if err != nil {
			return opMsgSection{}, 0, err
		}

		docs = append(docs, doc)
		docsBytes = docsBytes[len(doc):]
	}

	return opMsgSection{kind: 1, identifier: identifier, documents: docs}, size, nil
}

// body returns the first kind-0 section document — the command body.
func (m *opMsg) commandBody() (bson.Raw, bool) {
	for _, s := range m.sections {
		if s.kind == 0 && len(s.documents) == 1 {
			return s.documents[0], true
		}
	}

	return nil, false
}

// sequence returns the kind-1 document sequence with the given identifier.
func (m *opMsg) sequence(identifier string) ([]bson.Raw, bool) {
	for _, s := range m.sections {
		if s.kind == 1 && s.identifier == identifier {
			return s.documents, true
		}
	}

	return nil, false
}

// opQuery is a parsed legacy OP_QUERY (used only for the first hello).
type opQuery struct {
	fullCollectionName string
	query              bson.Raw
}

// parseOpQuery parses an OP_QUERY body (contract §1): int32 flags, cstring
// fullCollectionName, int32 numberToSkip, int32 numberToReturn, BSON query.
func parseOpQuery(body []byte) (*opQuery, error) {
	const skipReturnLen = 8

	if len(body) < 4 {
		return nil, ErrShortMessage
	}

	pos := 4 // skip flags

	nameEnd := indexByte(body[pos:], 0)
	if nameEnd < 0 {
		return nil, ErrShortMessage
	}

	name := string(body[pos : pos+nameEnd])
	pos += nameEnd + 1 + skipReturnLen

	if pos > len(body) {
		return nil, ErrShortMessage
	}

	q, err := readBSONDoc(body[pos:])
	if err != nil {
		return nil, err
	}

	return &opQuery{fullCollectionName: name, query: q}, nil
}

// readBSONDoc reads a length-prefixed BSON document from the prefix of b.
func readBSONDoc(b []byte) (bson.Raw, error) {
	const minDocSize = 5 // int32 length + trailing NUL

	if len(b) < 4 {
		return nil, ErrShortMessage
	}

	size := int(binary.LittleEndian.Uint32(b[0:4]))
	if size < minDocSize || size > len(b) {
		return nil, fmt.Errorf("%w: bson doc size %d", ErrShortMessage, size)
	}

	return bson.Raw(b[:size]), nil
}

// commandName returns the first key of a command document — the command name.
func commandName(doc bson.Raw) string {
	elems, err := doc.Elements()
	if err != nil || len(elems) == 0 {
		return ""
	}

	return elems[0].Key()
}

// buildOpMsgReply builds an OP_MSG reply with a single kind-0 section. The
// checksum and moreToCome flags are never set (contract §1/§6).
func buildOpMsgReply(requestID, responseTo int32, doc any) ([]byte, error) {
	docBytes, err := bson.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("mongodb: marshal reply: %w", err)
	}

	// header(16) + flags(4) + kind(1) + doc
	total := headerLen + 4 + 1 + len(docBytes)
	buf := make([]byte, total)

	writeHeader(buf, int32(total), requestID, responseTo, opCodeMsg)
	binary.LittleEndian.PutUint32(buf[16:20], 0) // flags
	buf[20] = 0                                  // section kind 0
	copy(buf[21:], docBytes)

	return buf, nil
}

// buildOpReply builds a legacy OP_REPLY answering an OP_QUERY hello
// (contract §1).
func buildOpReply(requestID, responseTo int32, doc any) ([]byte, error) {
	docBytes, err := bson.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("mongodb: marshal reply: %w", err)
	}

	// header(16) + responseFlags(4) + cursorID(8) + startingFrom(4) +
	// numberReturned(4) + doc
	const fixed = 4 + 8 + 4 + 4

	total := headerLen + fixed + len(docBytes)
	buf := make([]byte, total)

	writeHeader(buf, int32(total), requestID, responseTo, opCodeReply)
	binary.LittleEndian.PutUint32(buf[16:20], opReplyAwaitCapable) // responseFlags
	// cursorID (20:28) = 0, startingFrom (28:32) = 0
	binary.LittleEndian.PutUint32(buf[32:36], 1) // numberReturned
	copy(buf[36:], docBytes)

	return buf, nil
}

// writeHeader writes the 16-byte message header into buf.
func writeHeader(buf []byte, length, requestID, responseTo, opCode int32) {
	binary.LittleEndian.PutUint32(buf[0:4], uint32(length))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opCode))
}

// indexByte returns the index of the first occurrence of c in b, or -1.
func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}

	return -1
}

// lookupString returns a string field from a BSON document, or "".
func lookupString(doc bson.Raw, key string) string {
	if doc == nil {
		return ""
	}

	if s, ok := doc.Lookup(key).StringValueOK(); ok {
		return s
	}

	return ""
}

// lookupBool returns a bool field from a BSON document, or false.
func lookupBool(doc bson.Raw, key string) bool {
	if doc == nil {
		return false
	}

	if b, ok := doc.Lookup(key).BooleanOK(); ok {
		return b
	}

	return false
}
