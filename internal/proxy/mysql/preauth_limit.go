package mysql

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
)

// ErrPreAuthPacketTooLarge is returned when an unauthenticated client declares
// a packet payload larger than preAuthPacketCap.
var ErrPreAuthPacketTooLarge = errors.New("mysql: pre-auth packet exceeds the maximum length")

// preAuthPacketCap bounds the payload a client may declare before its handshake
// has completed. A real handshake response — username, auth response, initial
// database, connection attributes — is well under a kilobyte; 64 KiB is far
// more headroom than any driver needs.
//
// It exists because go-mysql sizes its read buffer from the 3-byte length in
// the packet header (packet.Conn.ReadPacketTo calls buf.Grow(length)) *before*
// a single payload byte arrives. Four bytes from an unauthenticated scanner
// therefore bought a 16 MiB allocation per connection, and the proxy port is
// exposed to the internet. The library gives no hook for this, so the limit is
// enforced under it, on the net.Conn it reads through.
const preAuthPacketCap = 64 * 1024

// mysqlPacketHeaderLen is the 3-byte payload length plus the 1-byte sequence id
// that prefix every MySQL protocol packet.
const mysqlPacketHeaderLen = 4

// clientSSLCapability is CLIENT_SSL in the capability flags a handshake
// response opens with. When the client sets it, the packet is a 32-byte
// SSLRequest and everything after it on this socket is TLS records, not MySQL
// frames — so the limiter has to stop parsing at that point.
const clientSSLCapability uint32 = 0x00000800

// preAuthLimitedConn is a net.Conn that watches MySQL packet framing on the
// read side and fails the connection when an unauthenticated peer declares an
// oversized payload. Once disarmed — by a completed handshake, or by an
// SSLRequest that turns the stream into TLS records — it is a pass-through.
//
// Only the read path is inspected; writes go straight to the wrapped conn.
type preAuthLimitedConn struct {
	net.Conn

	// armed is read on every Read and cleared from the session goroutine once
	// the handshake returns, so it is atomic. The parsing state below is
	// touched only inside Read, which the protocol never runs concurrently.
	armed atomic.Bool

	headerLen   int     // bytes of the current packet header seen so far
	header      [4]byte // the header being accumulated
	remaining   int     // payload bytes of the current packet still to pass
	packetIndex int     // how many complete packets have gone by
	capsLen     int     // capability-flag bytes captured from packet 0
	caps        [4]byte
}

// newPreAuthLimitedConn arms a limiter over conn.
func newPreAuthLimitedConn(conn net.Conn) *preAuthLimitedConn {
	c := &preAuthLimitedConn{Conn: conn}
	c.armed.Store(true)

	return c
}

// disarm turns the limiter into a pass-through. Called once the handshake has
// completed: past that point the peer is authenticated, the framing may switch
// to the compressed protocol, and the caps no longer apply.
func (c *preAuthLimitedConn) disarm() {
	c.armed.Store(false)
}

// Read passes bytes through, inspecting the packet framing while armed. A
// violation returns the error with no bytes, so the caller aborts rather than
// acting on a prefix of a frame we have already rejected.
func (c *preAuthLimitedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.armed.Load() {
		if scanErr := c.scan(p[:n]); scanErr != nil {
			return 0, scanErr
		}
	}

	return n, err
}

// scan walks the bytes just read, tracking packet boundaries and rejecting any
// header whose declared payload exceeds the cap.
func (c *preAuthLimitedConn) scan(buf []byte) error {
	for len(buf) > 0 {
		// finishPacket can disarm mid-buffer (an SSLRequest and its TLS
		// ClientHello routinely arrive in one read); the rest of this buffer
		// is no longer MySQL framing.
		if !c.armed.Load() {
			return nil
		}

		if c.remaining > 0 {
			buf = c.consumePayload(buf)

			continue
		}

		// Accumulate the 4-byte header, which may straddle two reads.
		take := min(mysqlPacketHeaderLen-c.headerLen, len(buf))
		copy(c.header[c.headerLen:], buf[:take])
		c.headerLen += take
		buf = buf[take:]

		if c.headerLen < mysqlPacketHeaderLen {
			return nil
		}

		length := int(c.header[0]) | int(c.header[1])<<8 | int(c.header[2])<<16
		if length > preAuthPacketCap {
			return fmt.Errorf("%w: %d > %d", ErrPreAuthPacketTooLarge, length, preAuthPacketCap)
		}

		c.headerLen = 0
		c.remaining = length

		// A zero-length packet completes right here.
		if c.remaining == 0 {
			c.finishPacket()
		}
	}

	return nil
}

// consumePayload skips over payload bytes of the packet in flight, capturing
// the capability flags of packet 0 on the way past, and returns the unconsumed
// remainder of buf.
func (c *preAuthLimitedConn) consumePayload(buf []byte) []byte {
	skip := min(c.remaining, len(buf))

	if c.packetIndex == 0 && c.capsLen < len(c.caps) {
		grab := min(len(c.caps)-c.capsLen, skip)
		copy(c.caps[c.capsLen:], buf[:grab])
		c.capsLen += grab
	}

	c.remaining -= skip

	if c.remaining == 0 {
		c.finishPacket()
	}

	return buf[skip:]
}

// finishPacket closes out a packet. After the client's first one, an
// SSLRequest disarms the limiter: the bytes that follow are a TLS handshake,
// and reading them as MySQL headers would reject a perfectly good connection.
func (c *preAuthLimitedConn) finishPacket() {
	if c.packetIndex == 0 && c.capsLen == len(c.caps) {
		caps := uint32(c.caps[0]) | uint32(c.caps[1])<<8 | uint32(c.caps[2])<<16 | uint32(c.caps[3])<<24
		if caps&clientSSLCapability != 0 {
			c.disarm()
		}
	}

	c.packetIndex++
}
