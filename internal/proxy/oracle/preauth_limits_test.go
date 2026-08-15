package oracle

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// measuredConn is a net.Conn that records the largest single Read request the
// parser made. That number is the allocation the parser sized from the wire,
// which is the thing under test: a pre-auth parser must never let four
// attacker-chosen bytes name an arbitrary buffer size.
type measuredConn struct {
	net.Conn

	maxRequested int
}

func (c *measuredConn) Read(p []byte) (int, error) {
	if len(p) > c.maxRequested {
		c.maxRequested = len(p)
	}

	return c.Conn.Read(p)
}

// serveTNSBytes feeds raw down a pipe and hands back a measured reading end.
func serveTNSBytes(t *testing.T, raw []byte) *measuredConn {
	t.Helper()

	clientSide, serverSide := net.Pipe()

	go func() {
		_, _ = clientSide.Write(raw)
		_ = clientSide.Close()
	}()

	t.Cleanup(func() { _ = serverSide.Close() })

	return &measuredConn{Conn: serverSide}
}

// TestReadTNSPacket_LengthIsStructurallyBounded pins the property that keeps
// the Oracle listener out of the pre-auth memory-bomb class: whatever eight
// header bytes an unauthenticated peer sends, the payload buffer readTNSPacket
// sizes from them cannot exceed maxTNSPacketSize.
//
// Both length encodings are bounded by construction — the legacy field is a
// uint16, and the v315+ 4-byte field is only consulted when the leading uint16
// reads as zero, which pins its top half to zero as well — so the explicit
// ErrTNSPacketTooLarge check downstream is belt-and-braces rather than the
// thing doing the work. This test is what would fail if someone "fixed" the
// v315+ branch to read a genuinely 32-bit length without adding a cap first.
func TestReadTNSPacket_LengthIsStructurallyBounded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		header []byte
	}{
		{"all length bits set", []byte{0xff, 0xff, 0xff, 0xff, byte(TNSPacketTypeConnect), 0, 0, 0}},
		{"legacy length at its maximum", []byte{0xff, 0xff, 0x00, 0x00, byte(TNSPacketTypeData), 0, 0, 0}},
		{"v315+ length at its maximum", []byte{0x00, 0x00, 0xff, 0xff, byte(TNSPacketTypeData), 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Header only: the peer names a size and sends nothing. The read
			// fails, but not before the parser has asked for its buffer.
			conn := serveTNSBytes(t, tc.header)

			pkt, err := readTNSPacket(conn)
			require.Nil(t, pkt)
			require.Error(t, err)
			require.LessOrEqual(t, conn.maxRequested, maxTNSPacketSize)
		})
	}
}

// TestReadExtendedConnectData_LengthIsStructurallyBounded covers the second
// length-prefixed field on the same unauthenticated connect packet: the
// extended connect data appended after a v315+ header. Its size prefix is a
// uint16, so it is bounded the same way.
func TestReadExtendedConnectData_LengthIsStructurallyBounded(t *testing.T) {
	t.Parallel()

	// A 20-byte payload whose connect-data offset points past what has been
	// read, which is what sends readTNSPacket into the extended-data path.
	payload := make([]byte, 20)
	payload[18] = 0xff
	payload[19] = 0xff

	raw := make([]byte, 0, tnsHeaderSize+len(payload)+2)
	raw = append(raw, 0x00, byte(tnsHeaderSize+len(payload)), 0x00, 0x00, byte(TNSPacketTypeConnect), 0, 0, 0)
	raw = append(raw, payload...)
	// The extended-data size prefix, at its uint16 maximum.
	raw = append(raw, 0xff, 0xff)

	conn := serveTNSBytes(t, raw)

	pkt, err := readTNSPacket(conn)
	require.Nil(t, pkt)
	require.Error(t, err)
	require.LessOrEqual(t, conn.maxRequested, maxTNSPacketSize)
}
