package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// TNSPacketType represents a TNS packet type code.
type TNSPacketType byte

// TNS packet type codes.
const (
	TNSPacketTypeConnect  TNSPacketType = 1
	TNSPacketTypeAccept   TNSPacketType = 2
	TNSPacketTypeRefuse   TNSPacketType = 3
	TNSPacketTypeRedirect TNSPacketType = 4
	TNSPacketTypeMarker   TNSPacketType = 5
	TNSPacketTypeData     TNSPacketType = 6
	TNSPacketTypeResend   TNSPacketType = 11
	TNSPacketTypeControl  TNSPacketType = 12
)

// TNS header size in bytes.
const tnsHeaderSize = 8

// TNS errors.
var (
	ErrTNSHeaderTooShort = errors.New("TNS header too short: need at least 8 bytes")
	ErrTNSPacketTooLarge = errors.New("TNS packet length exceeds maximum")
)

// Maximum TNS packet size (64KB should be more than enough; SDU is typically 32767).
const maxTNSPacketSize = 65535

// TNSPacket represents a single TNS protocol packet.
type TNSPacket struct {
	Type    TNSPacketType
	Flags   byte
	Length  uint16
	Payload []byte
	Raw     []byte // Original raw bytes (for forwarding without re-encoding)
}

// String returns a human-readable name for the packet type.
func (t TNSPacketType) String() string {
	switch t {
	case TNSPacketTypeConnect:
		return "Connect"
	case TNSPacketTypeAccept:
		return "Accept"
	case TNSPacketTypeRefuse:
		return "Refuse"
	case TNSPacketTypeRedirect:
		return "Redirect"
	case TNSPacketTypeMarker:
		return "Marker"
	case TNSPacketTypeData:
		return "Data"
	case TNSPacketTypeResend:
		return "Resend"
	case TNSPacketTypeControl:
		return "Control"
	default:
		return fmt.Sprintf("Unknown(%d)", t)
	}
}

// parseTNSHeader parses an 8-byte TNS header and returns a TNSPacket with metadata populated.
// The Payload field is not populated — only Length, Type, and Flags.
func parseTNSHeader(raw []byte) (*TNSPacket, error) {
	if len(raw) < tnsHeaderSize {
		return nil, ErrTNSHeaderTooShort
	}

	pkt := &TNSPacket{
		Length: binary.BigEndian.Uint16(raw[0:2]),
		Type:   TNSPacketType(raw[4]),
		Flags:  raw[5],
	}

	return pkt, nil
}

// encodeTNSPacket encodes a TNS packet with the given type and payload into a byte slice.
func encodeTNSPacket(typ TNSPacketType, payload []byte) []byte {
	totalLen := tnsHeaderSize + len(payload)
	buf := make([]byte, totalLen)

	binary.BigEndian.PutUint16(buf[0:2], uint16(totalLen)) // packet length
	// buf[2:4] = checksum (0x0000)
	buf[4] = byte(typ)
	// buf[5] = reserved/flags (0x00)
	// buf[6:8] = header checksum (0x0000)

	if len(payload) > 0 {
		copy(buf[tnsHeaderSize:], payload)
	}

	return buf
}

// readTNSPacket reads a complete TNS packet from a connection.
// Handles both legacy (2-byte length) and v315+ (4-byte length) packet formats.
//
// In TNS v315+, packets after Connect/Accept use a 4-byte length at bytes 0-3
// (the 2-byte length field reads as 0x0000). Connect packets still use the legacy
// 2-byte format but may have extended connect data appended after the header.
func readTNSPacket(conn net.Conn) (*TNSPacket, error) {
	// Read the 8-byte header
	header := make([]byte, tnsHeaderSize)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("failed to read TNS header: %w", err)
	}

	pkt, err := parseTNSHeader(header)
	if err != nil {
		return nil, err
	}

	// Determine packet length: 2-byte (legacy) or 4-byte (v315+)
	var packetLen int
	if pkt.Length > 0 {
		// Legacy format: 2-byte length at bytes 0-1
		packetLen = int(pkt.Length)
	} else {
		// v315+ format: 4-byte length at bytes 0-3
		packetLen = int(binary.BigEndian.Uint32(header[0:4]))
	}

	if packetLen < tnsHeaderSize {
		// Header-only packet (e.g., Resend with length=8)
		pkt.Raw = make([]byte, tnsHeaderSize)
		copy(pkt.Raw, header)

		return pkt, nil
	}

	if packetLen > maxTNSPacketSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrTNSPacketTooLarge, packetLen)
	}

	// Read the payload
	payloadLen := packetLen - tnsHeaderSize
	if payloadLen > 0 {
		pkt.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(conn, pkt.Payload); err != nil {
			return nil, fmt.Errorf("failed to read TNS payload: %w", err)
		}
	}

	// Store raw bytes for forwarding
	pkt.Raw = make([]byte, 0, packetLen)
	pkt.Raw = append(pkt.Raw, header...)
	pkt.Raw = append(pkt.Raw, pkt.Payload...)

	// For Connect packets with extended data (TNS v315+), the declared header length
	// only covers the metadata. The connect data is appended after.
	if pkt.Type == TNSPacketTypeConnect {
		if err := readExtendedConnectData(conn, pkt); err != nil {
			return nil, err
		}
	}

	return pkt, nil
}

// readExtendedConnectData reads additional connect data for TNS Connect packets
// where the header length only covers the metadata (TNS version >= 315).
func readExtendedConnectData(conn net.Conn, pkt *TNSPacket) error {
	if len(pkt.Payload) < 20 {
		return nil
	}

	connectDataOffset := int(binary.BigEndian.Uint16(pkt.Payload[18:20]))
	payloadNeeded := connectDataOffset - tnsHeaderSize
	if payloadNeeded < len(pkt.Payload) {
		return nil // Connect data fits within what we already read
	}

	// Read the first 2 bytes to get the actual size of the extended data
	sizeBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, sizeBuf); err != nil {
		return fmt.Errorf("failed to read extended connect data size: %w", err)
	}

	extendedLen := int(binary.BigEndian.Uint16(sizeBuf))
	if extendedLen < 2 || extendedLen > maxTNSPacketSize {
		return fmt.Errorf("%w: %d", ErrTNSPacketTooLarge, extendedLen)
	}

	// Read the remaining extended data (extendedLen includes the 2-byte size field)
	remaining := make([]byte, extendedLen-2)
	if _, err := io.ReadFull(conn, remaining); err != nil {
		return fmt.Errorf("failed to read extended connect data: %w", err)
	}

	pkt.Payload = append(pkt.Payload, sizeBuf...)
	pkt.Payload = append(pkt.Payload, remaining...)
	pkt.Raw = append(pkt.Raw, sizeBuf...)
	pkt.Raw = append(pkt.Raw, remaining...)

	return nil
}

// writeTNSPacket writes a complete TNS packet to a connection.
// If Raw bytes are available (from readTNSPacket), they are forwarded as-is
// to preserve the original wire format (important for TNS version >= 315).
func writeTNSPacket(conn net.Conn, pkt *TNSPacket) error {
	var data []byte
	if len(pkt.Raw) > 0 {
		data = pkt.Raw
	} else {
		data = encodeTNSPacket(pkt.Type, pkt.Payload)
	}

	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("failed to write TNS packet: %w", err)
	}

	return nil
}

// Encode encodes the packet into a byte slice (convenience method).
func (p *TNSPacket) Encode() []byte {
	return encodeTNSPacket(p.Type, p.Payload)
}
