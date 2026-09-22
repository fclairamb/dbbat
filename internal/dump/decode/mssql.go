package decode

import (
	"encoding/binary"
	"fmt"

	"github.com/fclairamb/dbbat/internal/dump"
)

// TDS framing constants, mirroring internal/proxy/mssql/packet.go.
const (
	// tdsHeaderLen is the 8-byte header: type, status, a big-endian length
	// counting the header itself, the SPID, a packet id and a window byte.
	tdsHeaderLen = 8

	// tdsStatusEOM marks the last packet of a message. Everything before it is
	// a fragment, so the splitter reassembles rather than reporting each
	// packet as a message of its own.
	tdsStatusEOM = 0x01

	// tdsMaxMessageLen is the proxy's own ceiling on a reassembled message.
	tdsMaxMessageLen = 16 * 1024 * 1024

	tdsTypeSQLBatch   = 0x01
	tdsTypeLegacyAuth = 0x02
	tdsTypeRPC        = 0x03
	tdsTypeReply      = 0x04
	tdsTypeAttention  = 0x06
	tdsTypeBulkLoad   = 0x07
	tdsTypeFedAuth    = 0x08
	tdsTypeTransMgr   = 0x0E
	tdsTypeLogin7     = 0x10
	tdsTypeSSPI       = 0x11
	tdsTypePrelogin   = 0x12
)

// mssqlStream is one direction's reassembly state: the bytes not yet cut into
// packets, and the message being built out of them.
type mssqlStream struct {
	buf     []byte
	message []byte
	msgType byte
	packets int
}

// mssqlSplitter cuts both directions of a SQL Server session into messages.
//
// TDS is the one protocol here whose framing unit is not the message: a packet
// carries at most `packet size` bytes and a long batch is chopped across
// several, the last one carrying the EOM status bit. So the splitter
// reassembles first and decodes second, and a message that spans four packets
// is one trace line, timed by the packet that completed it.
//
// The response is a token stream, walked token by token. The walk is total for
// the tokens that carry their own length and, through a TYPE_INFO decoder, for
// COLMETADATA and the rows that follow it — which is what makes `Row(3 cols)`
// possible. A token it cannot frame stops the walk and says so rather than
// guessing a length and mis-reading everything after it.
//
// Redaction follows the PostgreSQL splitter: statement text is printed, row
// values collapse to counts unless Options.ShowRows is set, and LOGIN7
// credentials and SSPI blobs are never printed either way.
type mssqlSplitter struct {
	opts Options

	client mssqlStream
	server mssqlStream

	// columns is the shape declared by the last COLMETADATA, which is what
	// lets the following rows be framed at all.
	columns []mssqlColumn

	// awaitingPrelogin marks that the client sent a PRELOGIN whose answer is
	// still owed. A PRELOGIN response travels under the TabularResult packet
	// type but is an option list, not a token stream.
	awaitingPrelogin bool
}

func newMSSQLSplitter(opts Options) *mssqlSplitter {
	return &mssqlSplitter{opts: opts}
}

// Feed consumes one packet and returns the messages it completed.
func (m *mssqlSplitter) Feed(packet *dump.Packet) ([]Message, error) {
	stream := &m.client
	if packet.Direction == dump.DirServerToClient {
		stream = &m.server
	}

	stream.buf = append(stream.buf, packet.Data...)

	var out []Message

	for {
		msgType, body, eom, ok, err := cutTDSPacket(&stream.buf)
		if err != nil {
			return out, err
		}

		if !ok {
			return out, nil
		}

		if stream.packets == 0 {
			stream.msgType = msgType
		}

		stream.packets++
		stream.message = append(stream.message, body...)

		if len(stream.message) > tdsMaxMessageLen {
			return out, fmt.Errorf("%w: TDS message past %d bytes", ErrOutOfSync, tdsMaxMessageLen)
		}

		if !eom {
			continue
		}

		for _, text := range m.renderMessage(packet.Direction, stream.msgType, stream.message) {
			out = append(out, Message{packet.RelativeNs, packet.Direction, text})
		}

		stream.message = nil
		stream.msgType = 0
		stream.packets = 0
	}
}

// cutTDSPacket cuts one packet off the front of buf, returning its type, its
// body and whether it ends the message.
func cutTDSPacket(buf *[]byte) (byte, []byte, bool, bool, error) {
	if len(*buf) < tdsHeaderLen {
		return 0, nil, false, false, nil
	}

	length := int(binary.BigEndian.Uint16((*buf)[2:4]))
	if length < tdsHeaderLen {
		return 0, nil, false, false, fmt.Errorf("%w: TDS packet length %d", ErrOutOfSync, length)
	}

	if len(*buf) < length {
		return 0, nil, false, false, nil
	}

	msgType := (*buf)[0]
	eom := (*buf)[1]&tdsStatusEOM != 0
	body := (*buf)[tdsHeaderLen:length]
	*buf = (*buf)[length:]

	return msgType, body, eom, true, nil
}

// renderMessage turns one reassembled TDS message into trace lines. A request
// is one line; a response is one line per token.
func (m *mssqlSplitter) renderMessage(direction byte, msgType byte, body []byte) []string {
	if direction == dump.DirServerToClient {
		return m.renderResponse(msgType, body)
	}

	return m.renderRequest(msgType, body)
}

func (m *mssqlSplitter) renderRequest(msgType byte, body []byte) []string {
	switch msgType {
	case tdsTypePrelogin:
		m.awaitingPrelogin = true

		return []string{formatTDSPrelogin(body)}
	case tdsTypeLogin7:
		// The password sits here, obfuscated by a transform that is not
		// encryption. It is never printed, and --rows does not lift this.
		return []string{formatTDSLogin7(body)}
	case tdsTypeSQLBatch:
		return []string{"SQLBatch " + quote(ucs2String(skipTDSAllHeaders(body)))}
	case tdsTypeRPC:
		return formatTDSRPC(skipTDSAllHeaders(body), m.opts)
	case tdsTypeAttention:
		return []string{"Attention"}
	case tdsTypeSSPI, tdsTypeLegacyAuth, tdsTypeFedAuth:
		return []string{fmt.Sprintf("%s(%d bytes, redacted)", tdsPacketTypeName(msgType), len(body))}
	case tdsTypeBulkLoad:
		// Bulk-load data is table content: counted, never printed.
		return []string{fmt.Sprintf("BulkLoad(%d bytes)", len(body))}
	case tdsTypeTransMgr:
		return []string{formatTDSTransactionManager(body)}
	default:
		return []string{fmt.Sprintf("%s(%d bytes)", tdsPacketTypeName(msgType), len(body))}
	}
}

func (m *mssqlSplitter) renderResponse(msgType byte, body []byte) []string {
	if msgType != tdsTypeReply {
		return []string{fmt.Sprintf("%s(%d bytes)", tdsPacketTypeName(msgType), len(body))}
	}

	if m.awaitingPrelogin {
		m.awaitingPrelogin = false

		return []string{formatTDSPrelogin(body)}
	}

	return m.walkTokens(body)
}

// skipTDSAllHeaders steps over the ALL_HEADERS block a request may carry: a
// DWORD total length counting itself, then the headers.
func skipTDSAllHeaders(body []byte) []byte {
	const lenSize = 4

	if len(body) < lenSize {
		return body
	}

	total := int(binary.LittleEndian.Uint32(body[:lenSize]))
	if total < lenSize || total > len(body) {
		return body
	}

	return body[total:]
}
