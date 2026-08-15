package mysql

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// mysqlPacket renders a MySQL protocol packet: 3-byte little-endian payload
// length, sequence id, payload.
func mysqlPacket(seq byte, payload []byte) []byte {
	n := len(payload)

	out := make([]byte, 0, mysqlPacketHeaderLen+n)
	out = append(out, byte(n), byte(n>>8), byte(n>>16), seq)

	return append(out, payload...)
}

// oversizedHeader renders just the 4-byte header of a packet declaring n
// payload bytes — the bytes a scanner sends to buy an allocation without
// paying for it.
func oversizedHeader(n int) []byte {
	return []byte{byte(n), byte(n >> 8), byte(n >> 16), 0}
}

// readThrough drives a limiter over the given client bytes and returns what
// came out plus the terminating error.
func readThrough(t *testing.T, clientBytes []byte) ([]byte, error) {
	t.Helper()

	clientSide, serverSide := net.Pipe()

	go func() {
		_, _ = clientSide.Write(clientBytes)
		_ = clientSide.Close()
	}()

	defer func() { _ = serverSide.Close() }()

	limited := newPreAuthLimitedConn(serverSide)

	out, err := io.ReadAll(limited)

	return out, err
}

// TestPreAuthLimit_RejectsOversizedHeader is the regression test for the
// pre-auth amplification: go-mysql grows its buffer to the declared length
// before reading a byte of payload, so 0xffffff in the header is 16 MiB of
// allocation for four bytes of traffic. The limiter must refuse on the header
// alone.
func TestPreAuthLimit_RejectsOversizedHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		declare int
	}{
		{"maximum payload length", 0xffffff},
		{"one byte over the cap", preAuthPacketCap + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := readThrough(t, oversizedHeader(tc.declare))
			require.ErrorIs(t, err, ErrPreAuthPacketTooLarge)
		})
	}
}

// TestPreAuthLimit_RejectsHeaderSplitAcrossReads pins that the check survives
// a header arriving one byte at a time — the trivial evasion.
func TestPreAuthLimit_RejectsHeaderSplitAcrossReads(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()

	go func() {
		for _, b := range oversizedHeader(0xffffff) {
			_, _ = clientSide.Write([]byte{b})
		}
		_ = clientSide.Close()
	}()

	defer func() { _ = serverSide.Close() }()

	limited := newPreAuthLimitedConn(serverSide)

	_, err := io.ReadAll(limited)
	require.ErrorIs(t, err, ErrPreAuthPacketTooLarge)
}

// TestPreAuthLimit_PassesRealHandshake pins the other side of the bound: an
// ordinary handshake response, then an auth-switch reply, flow through
// untouched.
func TestPreAuthLimit_PassesRealHandshake(t *testing.T) {
	t.Parallel()

	// Capability flags without CLIENT_SSL, then a plausible tail.
	handshake := make([]byte, 200)
	handshake[0] = 0x0d // CLIENT_LONG_PASSWORD | CLIENT_LONG_FLAG | CLIENT_CONNECT_WITH_DB

	stream := mysqlPacket(1, handshake)
	stream = append(stream, mysqlPacket(3, []byte("scrambled"))...)

	out, err := readThrough(t, stream)
	require.NoError(t, err)
	require.Equal(t, stream, out)
}

// TestPreAuthLimit_DisarmsAfterSSLRequest is the correctness guard, not a
// security one: after a 32-byte SSLRequest the socket carries TLS records, and
// a TLS ClientHello read as a MySQL header decodes to a bogus multi-megabyte
// length. The limiter has to stop parsing, or every TLS client would be
// rejected.
func TestPreAuthLimit_DisarmsAfterSSLRequest(t *testing.T) {
	t.Parallel()

	sslRequest := make([]byte, 32)
	sslRequest[1] = 0x08 // CLIENT_SSL, little-endian byte 1

	// 0x16 0x03 0x01 is the start of a TLS handshake record; as a MySQL
	// header it declares 0x010316 = 66326 payload bytes, over the cap.
	stream := mysqlPacket(1, sslRequest)
	stream = append(stream, 0x16, 0x03, 0x01, 0x02, 0x00)

	out, err := readThrough(t, stream)
	require.NoError(t, err)
	require.Equal(t, stream, out)
}

// TestPreAuthLimit_DisarmedIsPassThrough pins that a completed handshake lifts
// the cap — post-auth traffic legitimately uses the full 16 MiB packet size.
func TestPreAuthLimit_DisarmedIsPassThrough(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()

	go func() {
		_, _ = clientSide.Write(oversizedHeader(0xffffff))
		_ = clientSide.Close()
	}()

	defer func() { _ = serverSide.Close() }()

	limited := newPreAuthLimitedConn(serverSide)
	limited.disarm()

	out, err := io.ReadAll(limited)
	require.NoError(t, err)
	require.Len(t, out, mysqlPacketHeaderLen)
}
