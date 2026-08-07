package mssql

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// TDS packet types (MS-TDS 2.2.3.1.1).
const (
	packetTypeSQLBatch   byte = 0x01
	packetTypeLegacyAuth byte = 0x02 // pre-TDS7 login, refused by this proxy
	packetTypeRPC        byte = 0x03
	packetTypeReply      byte = 0x04 // tabular result (server -> client)
	packetTypeAttention  byte = 0x06
	packetTypeBulkLoad   byte = 0x07
	packetTypeFedAuth    byte = 0x08
	packetTypeTransMgr   byte = 0x0E
	packetTypeLogin7     byte = 0x10
	packetTypeSSPI       byte = 0x11
	packetTypePrelogin   byte = 0x12
)

// packetTypeName renders a packet type for diagnostics. Unknown types come back
// as hex rather than being swallowed, because "the client sent 0x2f" is the
// only useful thing to say about a desynchronised stream.
func packetTypeName(t byte) string {
	switch t {
	case packetTypeSQLBatch:
		return "SQLBatch"
	case packetTypeLegacyAuth:
		return "PreTDS7Login"
	case packetTypeRPC:
		return "RPC"
	case packetTypeReply:
		return "TabularResult"
	case packetTypeAttention:
		return "Attention"
	case packetTypeBulkLoad:
		return "BulkLoad"
	case packetTypeFedAuth:
		return "FedAuthToken"
	case packetTypeTransMgr:
		return "TransactionManager"
	case packetTypeLogin7:
		return "LOGIN7"
	case packetTypeSSPI:
		return "SSPI"
	case packetTypePrelogin:
		return "PRELOGIN"
	default:
		return "0x" + strconv.FormatUint(uint64(t), 16)
	}
}

// TDS packet status bits (MS-TDS 2.2.3.1.2).
const (
	statusNormal byte = 0x00
	statusEOM    byte = 0x01
	statusIgnore byte = 0x02
	// statusResetConnection / statusResetConnectionSkipTran are set by a
	// connection pool reusing a physical connection for a new logical session:
	// they ask the server to drop temp tables, SET options and cursors first.
	// The proxy must carry them upstream, or a pooled client would inherit the
	// previous session's state through dbbat while getting a clean one when
	// connecting directly.
	statusResetConnection         byte = 0x08
	statusResetConnectionSkipTran byte = 0x10
)

// relayedStatusBits are the status bits the relay carries from a client message
// onto the message it forwards upstream. EOM and the packet id are the
// framing's own business and are recomputed; IGNORE never reaches the forward
// path (ReadMessage reports it as an error).
const relayedStatusBits = statusResetConnection | statusResetConnectionSkipTran

// Packet sizing. The header is fixed at 8 bytes; the length field covers the
// header *and* the payload, so it is bounded by what fits in a uint16.
const (
	packetHeaderSize = 8
	// defaultPacketSize is the size every connection starts at, before LOGIN7
	// asks for something else (MS-TDS 2.2.6.4). PRELOGIN and the encapsulated
	// TLS handshake always run at this size.
	defaultPacketSize = 4096
	// minPacketSize / maxPacketSize bracket what a client may negotiate.
	minPacketSize = 512
	maxPacketSize = 32767
	// maxMessageSize caps a reassembled message. TDS itself puts no limit on
	// how many packets a message spans, so without this a peer could make the
	// proxy allocate without bound by never setting EOM.
	maxMessageSize = 16 * 1024 * 1024
)

// Framing errors. All static, so callers can match on them.
var (
	// ErrShortHeader — the peer closed (or the stream desynchronised) partway
	// through an 8-byte packet header.
	ErrShortHeader = errors.New("mssql: truncated TDS packet header")
	// ErrShortPayload — the header promised more bytes than arrived.
	ErrShortPayload = errors.New("mssql: truncated TDS packet payload")
	// ErrBadPacketLength — the length field is smaller than the header it
	// includes, so the packet cannot be parsed at all.
	ErrBadPacketLength = errors.New("mssql: invalid TDS packet length")
	// ErrMessageTooLarge — a message spanned more than maxMessageSize bytes
	// without ever setting the EOM status bit.
	ErrMessageTooLarge = errors.New("mssql: TDS message exceeds the maximum reassembled size")
	// ErrMixedMessageTypes — packets belonging to one message disagreed on
	// their type byte.
	ErrMixedMessageTypes = errors.New("mssql: TDS message packets have inconsistent types")
	// ErrMessageIgnored — the terminal packet carried the IGNORE status bit,
	// meaning the sender wants the whole message discarded.
	ErrMessageIgnored = errors.New("mssql: TDS message marked IGNORE by the sender")
	// ErrPacketPending — a streaming packet was left open when the caller
	// asked for something that needs a clean message boundary.
	ErrPacketPending = errors.New("mssql: a TDS packet is still open")
)

// packetHeader is the 8-byte TDS packet header (MS-TDS 2.2.3.1). Every
// multi-byte field is big-endian ("network order"), unlike the little-endian
// payloads that follow it — a persistent source of TDS bugs.
type packetHeader struct {
	Type     byte
	Status   byte
	Length   uint16 // header + payload
	SPID     uint16
	PacketID byte
	Window   byte
}

// encode writes the header into an 8-byte array.
func (h packetHeader) encode() [packetHeaderSize]byte {
	var buf [packetHeaderSize]byte

	buf[0] = h.Type
	buf[1] = h.Status
	binary.BigEndian.PutUint16(buf[2:4], h.Length)
	binary.BigEndian.PutUint16(buf[4:6], h.SPID)
	buf[6] = h.PacketID
	buf[7] = h.Window

	return buf
}

// decodeHeader parses an 8-byte header. It rejects a length field that does not
// even cover the header, which would otherwise underflow the payload size.
func decodeHeader(buf []byte) (packetHeader, error) {
	if len(buf) < packetHeaderSize {
		return packetHeader{}, ErrShortHeader
	}

	h := packetHeader{
		Type:     buf[0],
		Status:   buf[1],
		Length:   binary.BigEndian.Uint16(buf[2:4]),
		SPID:     binary.BigEndian.Uint16(buf[4:6]),
		PacketID: buf[6],
		Window:   buf[7],
	}

	if h.Length < packetHeaderSize {
		return packetHeader{}, fmt.Errorf("%w: %d", ErrBadPacketLength, h.Length)
	}

	return h, nil
}

// isEOM reports whether this packet ends the logical message.
func (h packetHeader) isEOM() bool {
	return h.Status&statusEOM != 0
}

// isIgnore reports whether the sender asked for the message to be discarded.
func (h packetHeader) isIgnore() bool {
	return h.Status&statusIgnore != 0
}

// packetRW frames TDS packets over a byte stream. It owns the outbound packet
// id counter and the negotiated packet size, and it reassembles inbound
// messages that span several packets.
//
// It is deliberately not safe for concurrent use: a TDS connection is a
// strictly alternating request/response stream, driven from one goroutine.
type packetRW struct {
	rw io.ReadWriter

	// packetSize is the full packet size (header included) currently in
	// force. PRELOGIN and the TLS handshake run at defaultPacketSize; LOGIN7
	// may raise it for the rest of the session.
	packetSize int

	// outPacketID is the sender's packet counter. It restarts at 1 for every
	// message and increments modulo 256 within it (MS-TDS 2.2.3.1.4).
	outPacketID byte

	// spid is echoed in outbound headers. Real servers put the session id
	// here; the proxy has no upstream session yet, so it stays 0 until stage 2
	// can mirror the upstream's.
	spid uint16

	// pending holds the payload of a packet opened by beginPacket and not yet
	// finished.
	pending []byte
	// pendingType is the packet type of the open streaming packet.
	pendingType byte
	// pendingOpen distinguishes "open but nothing written" from "not open",
	// which len(pending) alone cannot.
	pendingOpen bool

	// hdrBuf is scratch for reading headers, so a read loop does not allocate
	// one per packet.
	hdrBuf [packetHeaderSize]byte

	// peeked / peekedOK hold one byte read ahead of the next packet header by
	// peekType and not yet consumed. It exists for exactly one caller — the
	// encapsulated TLS handshake, which has to tell a TDS header from a raw TLS
	// record before committing to parsing either.
	peeked   byte
	peekedOK bool

	// lastReadStatus is the status byte of the first packet of the most
	// recently read message. The relay reads it to carry RESETCONNECTION
	// through; EOM and IGNORE are already handled by the framing.
	lastReadStatus byte

	// tapRead / tapWrite, when set, receive every raw packet (header included)
	// this codec reads and writes. The session capture uses them, which is why
	// they sit here rather than around the socket: on an encrypted client leg a
	// tap below TLS would only ever record ciphertext.
	//
	// They are installed once, before the relay's goroutines start. Read and
	// write are then driven from different goroutines — safe because the two
	// paths share no mutable state — so a tap that writes to a shared sink must
	// do its own locking.
	tapRead  func([]byte)
	tapWrite func([]byte)
}

// setTaps installs the capture callbacks. Either may be nil.
func (p *packetRW) setTaps(onRead, onWrite func([]byte)) {
	p.tapRead = onRead
	p.tapWrite = onWrite
}

// newPacketRW wraps a stream in the TDS framing codec.
func newPacketRW(rw io.ReadWriter) *packetRW {
	return &packetRW{rw: rw, packetSize: defaultPacketSize}
}

// SetPacketSize adopts the packet size a client asked for in LOGIN7, clamped to
// the range TDS allows. Out-of-range values are clamped rather than rejected,
// which is what SQL Server does.
func (p *packetRW) SetPacketSize(size int) {
	switch {
	case size < minPacketSize:
		p.packetSize = minPacketSize
	case size > maxPacketSize:
		p.packetSize = maxPacketSize
	default:
		p.packetSize = size
	}
}

// PacketSize returns the packet size currently in force.
func (p *packetRW) PacketSize() int {
	return p.packetSize
}

// setSPID sets the session id echoed in outbound headers.
func (p *packetRW) setSPID(spid uint16) {
	p.spid = spid
}

// payloadCapacity is how many payload bytes fit in one packet.
func (p *packetRW) payloadCapacity() int {
	return p.packetSize - packetHeaderSize
}

// readHeader fills hdrBuf with the next 8-byte header, consuming the peeked
// byte first when there is one.
func (p *packetRW) readHeader() error {
	rest := p.hdrBuf[:]

	if b, ok := p.takePeeked(); ok {
		p.hdrBuf[0] = b
		rest = p.hdrBuf[1:]
	}

	if _, err := io.ReadFull(p.rw, rest); err != nil {
		// A clean EOF on the *first* byte means the peer closed between
		// messages, which callers report as such. Once any byte of the header
		// has landed, the same EOF is a truncation.
		if errors.Is(err, io.EOF) && len(rest) == packetHeaderSize {
			return io.EOF
		}

		return fmt.Errorf("%w: %w", ErrShortHeader, err)
	}

	return nil
}

// peekType returns the first byte of the next packet without consuming it: the
// byte stays buffered and readPacket picks it up as the head of the header.
//
// A peeked byte must be either consumed by readPacket or handed to takePeeked;
// dropping it desynchronises the stream.
func (p *packetRW) peekType() (byte, error) {
	if p.peekedOK {
		return p.peeked, nil
	}

	var one [1]byte
	if _, err := io.ReadFull(p.rw, one[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}

		return 0, fmt.Errorf("%w: %w", ErrShortHeader, err)
	}

	p.peeked = one[0]
	p.peekedOK = true

	return p.peeked, nil
}

// takePeeked hands the peeked byte back to the caller and forgets it, for a
// caller that has decided the stream is no longer TDS-framed and will read the
// rest of it itself.
func (p *packetRW) takePeeked() (byte, bool) {
	if !p.peekedOK {
		return 0, false
	}

	p.peekedOK = false

	return p.peeked, true
}

// readPacket reads exactly one packet: header, then its payload.
func (p *packetRW) readPacket() (packetHeader, []byte, error) {
	if err := p.readHeader(); err != nil {
		return packetHeader{}, nil, err
	}

	hdr, err := decodeHeader(p.hdrBuf[:])
	if err != nil {
		return packetHeader{}, nil, err
	}

	size := int(hdr.Length) - packetHeaderSize
	if size == 0 {
		p.tapReadPacket(nil)

		return hdr, nil, nil
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(p.rw, payload); err != nil {
		return packetHeader{}, nil, fmt.Errorf("%w: %w", ErrShortPayload, err)
	}

	p.tapReadPacket(payload)

	return hdr, payload, nil
}

// tapReadPacket hands one inbound packet to the capture callback, putting the
// header back in front of the payload so the capture holds exactly what crossed
// the wire. hdrBuf still holds this packet's header — the next read is what
// overwrites it — so the frame is assembled here rather than at the call site.
func (p *packetRW) tapReadPacket(payload []byte) {
	if p.tapRead == nil {
		return
	}

	frame := make([]byte, 0, packetHeaderSize+len(payload))
	frame = append(frame, p.hdrBuf[:]...)
	frame = append(frame, payload...)

	p.tapRead(frame)
}

// ReadMessage reassembles a full logical message: packets of the same type are
// concatenated until one carries the EOM status bit.
func (p *packetRW) ReadMessage() (byte, []byte, error) {
	var (
		payload []byte
		msgType byte
		first   = true
	)

	for {
		hdr, chunk, err := p.readPacket()
		if err != nil {
			return 0, nil, err
		}

		if first {
			msgType = hdr.Type
			p.lastReadStatus = hdr.Status
			first = false
		} else if hdr.Type != msgType {
			return 0, nil, fmt.Errorf("%w: %s then %s",
				ErrMixedMessageTypes, packetTypeName(msgType), packetTypeName(hdr.Type))
		}

		if len(payload)+len(chunk) > maxMessageSize {
			return 0, nil, ErrMessageTooLarge
		}

		payload = append(payload, chunk...)

		if !hdr.isEOM() {
			continue
		}

		if hdr.isIgnore() {
			return msgType, nil, ErrMessageIgnored
		}

		return msgType, payload, nil
	}
}

// WriteMessage frames a complete payload as one logical message, splitting it
// across as many packets as the negotiated size requires and setting EOM on the
// last one only.
//
// An empty payload still produces one header-only packet with EOM, which is a
// legal (and occasionally required) TDS message.
func (p *packetRW) WriteMessage(msgType byte, payload []byte) error {
	return p.WriteMessageWithStatus(msgType, payload, statusNormal)
}

// WriteMessageWithStatus is WriteMessage with extra status bits ORed into the
// *first* packet of the message, which is where MS-TDS puts RESETCONNECTION.
func (p *packetRW) WriteMessageWithStatus(msgType byte, payload []byte, extra byte) error {
	if p.pendingOpen {
		return ErrPacketPending
	}

	p.outPacketID = 1
	capacity := p.payloadCapacity()

	for first := true; first || len(payload) > 0; first = false {
		chunk := payload
		if len(chunk) > capacity {
			chunk = chunk[:capacity]
		}

		payload = payload[len(chunk):]

		status := statusNormal
		if len(payload) == 0 {
			status = statusEOM
		}

		if first {
			status |= extra
		}

		if err := p.writePacket(msgType, status, chunk); err != nil {
			return err
		}
	}

	return nil
}

// writePacket emits a single packet and advances the packet id.
func (p *packetRW) writePacket(msgType, status byte, payload []byte) error {
	hdr := packetHeader{
		Type:     msgType,
		Status:   status,
		Length:   uint16(packetHeaderSize + len(payload)),
		SPID:     p.spid,
		PacketID: p.outPacketID,
	}

	p.outPacketID++

	buf := make([]byte, 0, packetHeaderSize+len(payload))
	encoded := hdr.encode()
	buf = append(buf, encoded[:]...)
	buf = append(buf, payload...)

	if _, err := p.rw.Write(buf); err != nil {
		return fmt.Errorf("mssql: write TDS packet: %w", err)
	}

	if p.tapWrite != nil {
		p.tapWrite(buf)
	}

	return nil
}

// ForwardPacket relays one packet of a message received from the other side,
// keeping the peer's type and status bits (EOM, IGNORE, RESETCONNECTION) but
// re-stamping the SPID and the packet id, which are per-connection.
//
// It exists because a response is one logical TDS message that can be
// arbitrarily large — a result set is not something a proxy may reassemble in
// memory before passing it on — so the relay forwards a response packet at a
// time. Pass start=true for the first packet of a message so the packet id
// counter restarts, as MS-TDS 2.2.3.1.4 requires.
func (p *packetRW) ForwardPacket(msgType, status byte, payload []byte, start bool) error {
	if p.pendingOpen {
		return ErrPacketPending
	}

	if start {
		p.outPacketID = 1
	}

	return p.writePacket(msgType, status, payload)
}

// beginPacket opens a streaming message of the given type. Writes accumulate
// until the payload fills a packet (flushed without EOM) or finishPacket is
// called (flushed with EOM).
//
// This exists for the encapsulated TLS handshake, which hands crypto/tls a
// net.Conn and therefore cannot know a flight's length up front.
func (p *packetRW) beginPacket(msgType byte) {
	p.pendingType = msgType
	p.pendingOpen = true
	p.outPacketID = 1
	p.pending = p.pending[:0]
}

// packetOpen reports whether a streaming packet is currently open.
func (p *packetRW) packetOpen() bool {
	return p.pendingOpen
}

// WriteStream appends to the open streaming packet, flushing whole packets
// (without EOM) as the buffer fills.
func (p *packetRW) WriteStream(data []byte) (int, error) {
	if !p.pendingOpen {
		p.beginPacket(p.pendingType)
	}

	written := 0
	capacity := p.payloadCapacity()

	for len(data) > 0 {
		room := capacity - len(p.pending)
		if room <= 0 {
			if err := p.writePacket(p.pendingType, statusNormal, p.pending); err != nil {
				return written, err
			}

			p.pending = p.pending[:0]

			continue
		}

		take := min(room, len(data))

		p.pending = append(p.pending, data[:take]...)
		data = data[take:]
		written += take
	}

	return written, nil
}

// finishPacket flushes the open streaming packet with the EOM bit, closing the
// message. It is a no-op when nothing is open.
func (p *packetRW) finishPacket() error {
	if !p.pendingOpen {
		return nil
	}

	payload := p.pending
	p.pending = p.pending[:0]
	p.pendingOpen = false

	return p.writePacket(p.pendingType, statusEOM, payload)
}
