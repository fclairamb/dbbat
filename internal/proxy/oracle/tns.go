package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// TNS packet type codes.
type TNSPacketType byte

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

	if pkt.Length < tnsHeaderSize {
		// Header-only packet (no payload)
		return pkt, nil
	}

	if pkt.Length > maxTNSPacketSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrTNSPacketTooLarge, pkt.Length)
	}

	// Read the payload
	payloadLen := int(pkt.Length) - tnsHeaderSize
	if payloadLen > 0 {
		pkt.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(conn, pkt.Payload); err != nil {
			return nil, fmt.Errorf("failed to read TNS payload: %w", err)
		}
	}

	return pkt, nil
}

// writeTNSPacket writes a complete TNS packet to a connection.
func writeTNSPacket(conn net.Conn, pkt *TNSPacket) error {
	raw := encodeTNSPacket(pkt.Type, pkt.Payload)
	if _, err := conn.Write(raw); err != nil {
		return fmt.Errorf("failed to write TNS packet: %w", err)
	}

	return nil
}

// Encode encodes the packet into a byte slice (convenience method).
func (p *TNSPacket) Encode() []byte {
	return encodeTNSPacket(p.Type, p.Payload)
}
