package decode

import (
	"encoding/binary"
	"fmt"

	"github.com/fclairamb/dbbat/internal/dump"
)

// TNS framing constants, mirroring internal/proxy/oracle/tns.go.
const (
	// tnsHeaderLen is the header every TNS packet starts with: a length, a
	// checksum, the packet type, a flag byte and a header checksum.
	tnsHeaderLen = 8

	// tnsMaxPacketSize is the proxy's own ceiling, and the bound that catches
	// a desynchronised stream before it allocates.
	tnsMaxPacketSize = 65535

	// tnsConnectDataOffsetPos is where a Connect packet declares where its
	// connect descriptor starts, counted from the top of the packet. When that
	// lands past the declared length, the descriptor is appended after the
	// packet with a 2-byte size of its own — which is why a Connect is the one
	// packet whose length field does not bound it.
	tnsConnectDataOffsetPos = 18

	// tnsDataFlagsLen is the 2-byte prefix of every Data payload, ahead of the
	// TTC message type.
	tnsDataFlagsLen = 2
)

// TNS packet types, as the wire numbers them.
//
// internal/proxy/oracle/tns.go numbers four of these one slot low — its
// `TNSPacketTypeRedirect` is the wire's Refuse, its `Marker` is Redirect and
// its `Control` is Marker — and stays self-consistent by using its own names
// throughout. A decoder is read against Wireshark and against the spec, so it
// names them the way the wire does.
const (
	tnsTypeConnect   = 1
	tnsTypeAccept    = 2
	tnsTypeAck       = 3
	tnsTypeRefuse    = 4
	tnsTypeRedirect  = 5
	tnsTypeData      = 6
	tnsTypeNull      = 7
	tnsTypeAbort     = 9
	tnsTypeResend    = 11
	tnsTypeMarker    = 12
	tnsTypeAttention = 13
	tnsTypeControl   = 14
)

// oracleSplitter cuts both directions of an Oracle session into messages.
//
// One line per TNS packet, named by its type and — for a Data packet, which is
// all of a session past the handshake — by the TTC message type and function
// code inside it. That is the level at which an Oracle capture is actually
// read: which call the client made, in what order, and how long the answer
// took.
//
// It deliberately stops there. Locating the statement text inside a TTC exec
// frame is a byte-exact problem the proxy solves with a dedicated locator
// (internal/proxy/oracle/ttc_statement_rewrite.go) that certifies each client
// shape before touching it; a second, looser implementation living here would
// be the drift that makes one of them wrong. Statement text is in the queries
// table, indexed by the same connection uid the capture is named after.
//
// Authentication payloads — the O5LOGON session key exchange and the phase-2
// verifier — are named and never printed, in either direction, and --rows does
// not lift that.
//
// One known limit, worth knowing before a line is trusted: a TTC message
// longer than what the peer writes in one go is split across several Data
// packets, and a continuation packet carries its own data flags followed by
// raw bytes — no marker, no repeated message type. Telling it apart from a new
// message means tracking each TTC shape's declared length, which is what
// internal/proxy/oracle/reassembly.go does with per-shape shortfall rules. So
// a continuation is named by whatever its first byte happens to be. It shows
// up on the OCI clients (sqlplus, ojdbc) on the long ODTYPES table and on big
// results; the thin drivers in testdata/ never fragment.
type oracleSplitter struct {
	opts Options

	clientBuf, serverBuf []byte

	// authPending marks that the client's last call was an authentication
	// step, so the server's answer carries the other half of it.
	authPending bool
}

func newOracleSplitter(opts Options) *oracleSplitter {
	return &oracleSplitter{opts: opts}
}

// Feed consumes one packet and returns the messages it completed.
func (o *oracleSplitter) Feed(packet *dump.Packet) ([]Message, error) {
	buf := &o.clientBuf
	if packet.Direction == dump.DirServerToClient {
		buf = &o.serverBuf
	}

	*buf = append(*buf, packet.Data...)

	var out []Message

	for {
		raw, ok, err := cutTNSPacket(buf)
		if err != nil {
			return out, err
		}

		if !ok {
			return out, nil
		}

		out = append(out, Message{packet.RelativeNs, packet.Direction, o.render(packet.Direction, raw)})
	}
}

// cutTNSPacket cuts one TNS packet off the front of buf. ok is false when the
// packet is not fully buffered yet.
//
// Two length encodings share the header. Up to TNS 315 the length is the
// 2-byte field at [0:2]; from 315 it is the 4-byte field at [0:4] and the
// 2-byte one reads as zero. Which one is in force is decided per packet by
// that zero, exactly as the proxy's reader does it, so a capture that switches
// mid-session (Connect and Accept are always legacy) is read correctly.
func cutTNSPacket(buf *[]byte) ([]byte, bool, error) {
	if len(*buf) < tnsHeaderLen {
		return nil, false, nil
	}

	length := int(binary.BigEndian.Uint16((*buf)[:2]))
	if length == 0 {
		length = int(binary.BigEndian.Uint32((*buf)[:4]))
	}

	if length < tnsHeaderLen || length > tnsMaxPacketSize {
		return nil, false, fmt.Errorf("%w: TNS packet length %d", ErrOutOfSync, length)
	}

	if len(*buf) < length {
		return nil, false, nil
	}

	total := length

	if (*buf)[4] == tnsTypeConnect {
		extended, ok := tnsConnectTotalLen(*buf, length)
		if !ok {
			return nil, false, nil
		}

		total = extended
	}

	if len(*buf) < total {
		return nil, false, nil
	}

	raw := (*buf)[:total]
	*buf = (*buf)[total:]

	return raw, true, nil
}

// tnsConnectTotalLen resolves the one packet whose declared length may cover
// only its metadata: from TNS 315 a Connect's descriptor is appended after the
// header block, prefixed by a 2-byte size that counts itself.
func tnsConnectTotalLen(buf []byte, length int) (int, bool) {
	payload := buf[tnsHeaderLen:length]
	if len(payload) < tnsConnectDataOffsetPos+2 {
		return length, true
	}

	offset := int(binary.BigEndian.Uint16(payload[tnsConnectDataOffsetPos : tnsConnectDataOffsetPos+2]))
	if offset-tnsHeaderLen < len(payload) {
		return length, true
	}

	if len(buf) < length+2 {
		return 0, false
	}

	extended := int(binary.BigEndian.Uint16(buf[length : length+2]))
	if extended < 2 || extended > tnsMaxPacketSize {
		return length, true
	}

	return length + extended, true
}

// render turns one TNS packet into a trace line.
func (o *oracleSplitter) render(direction byte, raw []byte) string {
	packetType := raw[4]
	payload := raw[tnsHeaderLen:]

	if packetType != tnsTypeData {
		return formatTNSPacket(packetType, payload)
	}

	return o.renderData(direction, payload)
}

// renderData names the TTC call inside a Data packet: the 2-byte data flags,
// then the message type, then — for the two piggyback message types — the
// function code that says what the call actually is.
func (o *oracleSplitter) renderData(direction byte, payload []byte) string {
	if len(payload) < tnsDataFlagsLen {
		return "Data(empty)"
	}

	flags := binary.BigEndian.Uint16(payload[:tnsDataFlagsLen])

	if len(payload) == tnsDataFlagsLen {
		return fmt.Sprintf("Data(flags=0x%04x)", flags)
	}

	ttc := payload[tnsDataFlagsLen:]

	if direction == dump.DirServerToClient {
		return o.renderServerData(ttc, flags)
	}

	text, isAuth := formatTTCCall(ttc)
	o.authPending = isAuth

	return text + formatTNSFlagSuffix(flags)
}

// renderServerData keeps the server's half of an authentication exchange out
// of the trace: it carries the session key and the server's verifier, and it
// has no function code of its own to recognize it by.
func (o *oracleSplitter) renderServerData(ttc []byte, flags uint16) string {
	if o.authPending {
		o.authPending = false

		return fmt.Sprintf("%s (%d bytes, redacted)", ttcMessageName(ttc[0]), len(ttc)) +
			formatTNSFlagSuffix(flags)
	}

	text, _ := formatTTCCall(ttc)

	return text + formatTNSFlagSuffix(flags)
}
