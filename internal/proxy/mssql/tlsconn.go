package mssql

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ErrHandshakeWrongPacketType — during the encapsulated handshake the peer sent
// something other than a PRELOGIN-typed packet, which means the stream is out
// of step and nothing good can come of continuing.
var ErrHandshakeWrongPacketType = errors.New("mssql: non-PRELOGIN packet during the TLS handshake")

// ErrHandshakeBytesUnconsumed — the handshake finished with bytes still sitting
// in an inbound PRELOGIN message.
//
// TDS is strictly alternating, so a conforming client's last framed message
// holds exactly its final handshake flight and nothing else. Bytes left over
// mean the peer packed post-handshake TLS records into the same PRELOGIN
// message — records that belong to the TLS session and that pass-through mode
// will never see. Dropping them silently would corrupt the stream a few reads
// later, somewhere unrelated; failing here says what actually happened.
var ErrHandshakeBytesUnconsumed = errors.New(
	"mssql: TLS handshake ended with unread bytes in a PRELOGIN message")

// handshakeConn is the net.Conn handed to crypto/tls for TDS's encapsulated TLS
// handshake.
//
// TDS is the odd one out among the protocols dbbat proxies: the TLS handshake
// records do not travel over the raw socket, they travel *inside* TDS packets
// typed PRELOGIN (0x12). Once the handshake completes the stream switches to
// ordinary TLS records over the raw socket, with no TDS wrapper — and later,
// for ENCRYPT_OFF, back to cleartext TDS again.
//
// So this adapter has three states, and they must switch under the same
// tls.Conn, because crypto/tls holds on to whatever net.Conn it was built with:
//
//	framed      — reads/writes are wrapped in PRELOGIN packets (handshake)
//	passthrough — reads/writes go straight to the socket (TLS records)
//	reverted    — reads/writes go straight to the socket, TLS bypassed
//
// The one subtle rule is in Read: an open outbound packet is finished (with the
// EOM bit) before reading. A TLS flight is exactly "everything written since
// the last read", and crypto/tls never tells us where a flight ends — but it
// does tell us when it wants to read one back, which is the same boundary. Get
// this wrong and the peer waits forever for a packet that is sitting in a
// buffer: the failure mode is an opaque client hang, which is why this type is
// small and tested on its own.
type handshakeConn struct {
	// raw is the underlying socket. Deadlines, addresses and Close are
	// delegated to it in every state.
	raw net.Conn

	// pkt frames PRELOGIN packets while the handshake is in flight.
	pkt *packetRW

	// mu guards framed, which flips from the session goroutine (after
	// Handshake returns) while crypto/tls may still hold a reference.
	mu sync.Mutex
	// framed is true while the handshake is encapsulated.
	framed bool

	// inbound holds the payload of the PRELOGIN message currently being
	// served to crypto/tls, and how much of it has been consumed.
	inbound    []byte
	inboundPos int
}

// newHandshakeConn wraps a socket for the encapsulated handshake. The packetRW
// must be the session's, so the packet id counter and the negotiated size stay
// consistent across the switch.
func newHandshakeConn(raw net.Conn, pkt *packetRW) *handshakeConn {
	return &handshakeConn{raw: raw, pkt: pkt, framed: true}
}

// isFramed reports whether the adapter is still encapsulating.
func (c *handshakeConn) isFramed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.framed
}

// deactivate ends the encapsulation: any outbound packet still open is flushed
// with EOM first, then reads and writes become plain pass-through.
//
// The flush matters for TLS 1.3, where the server's last handshake action can
// be a write with no read after it. Under TLS 1.2 the final flight is always
// followed by a read, so the flush is a no-op — but relying on that would make
// a version bump silently deadlock.
func (c *handshakeConn) deactivate() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.framed {
		return nil
	}

	c.framed = false
	unconsumed := len(c.inbound) - c.inboundPos

	if err := c.pkt.finishPacket(); err != nil {
		return err
	}

	if unconsumed > 0 {
		return fmt.Errorf("%w: %d bytes", ErrHandshakeBytesUnconsumed, unconsumed)
	}

	return nil
}

// Read serves bytes from the inbound PRELOGIN message while framed, and
// straight from the socket once not.
func (c *handshakeConn) Read(b []byte) (int, error) {
	if !c.isFramed() {
		return c.raw.Read(b)
	}

	// A flight ends where the next read begins.
	if c.pkt.packetOpen() {
		if err := c.pkt.finishPacket(); err != nil {
			return 0, err
		}
	}

	for c.inboundPos >= len(c.inbound) {
		msgType, payload, err := c.pkt.ReadMessage()
		if err != nil {
			return 0, err
		}

		if msgType != packetTypePrelogin {
			return 0, ErrHandshakeWrongPacketType
		}

		// An empty PRELOGIN message carries no TLS bytes, and the loop
		// condition sends us straight back for the next one — reporting a
		// zero-length read instead would spin crypto/tls.
		c.inbound = payload
		c.inboundPos = 0
	}

	n := copy(b, c.inbound[c.inboundPos:])
	c.inboundPos += n

	return n, nil
}

// Write buffers into a PRELOGIN-typed packet while framed, and writes straight
// to the socket once not.
func (c *handshakeConn) Write(b []byte) (int, error) {
	if !c.isFramed() {
		return c.raw.Write(b)
	}

	if !c.pkt.packetOpen() {
		c.pkt.beginPacket(packetTypePrelogin)
	}

	return c.pkt.WriteStream(b)
}

// Close closes the underlying socket.
func (c *handshakeConn) Close() error {
	return c.raw.Close()
}

// LocalAddr returns the socket's local address.
func (c *handshakeConn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// RemoteAddr returns the socket's remote address.
func (c *handshakeConn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// SetDeadline sets both deadlines on the socket.
func (c *handshakeConn) SetDeadline(t time.Time) error {
	return c.raw.SetDeadline(t)
}

// SetReadDeadline sets the read deadline on the socket.
func (c *handshakeConn) SetReadDeadline(t time.Time) error {
	return c.raw.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline on the socket.
func (c *handshakeConn) SetWriteDeadline(t time.Time) error {
	return c.raw.SetWriteDeadline(t)
}

// Compile-time proof that the adapter really is a net.Conn — crypto/tls will
// not accept anything less.
var _ net.Conn = (*handshakeConn)(nil)

// revertibleConn is the stream the session reads TDS from after the handshake.
//
// For ENCRYPT_ON / ENCRYPT_REQ it is just the tls.Conn forever. For ENCRYPT_OFF
// the TLS session covers the LOGIN7 packet only: once that packet has been
// read, both sides drop back to cleartext TDS on the raw socket. Swapping the
// stream under the session is the whole point of the type.
type revertibleConn struct {
	mu     sync.Mutex
	active io.ReadWriter
	raw    net.Conn
}

// newRevertibleConn starts on the raw socket.
func newRevertibleConn(raw net.Conn) *revertibleConn {
	return &revertibleConn{active: raw, raw: raw}
}

// switchTo puts a new stream (in practice a *tls.Conn) in force.
func (c *revertibleConn) switchTo(rw io.ReadWriter) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.active = rw
}

// revert drops back to the raw socket. Idempotent.
func (c *revertibleConn) revert() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.active = c.raw
}

// current returns the stream in force.
func (c *revertibleConn) current() io.ReadWriter {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.active
}

// Read reads from whichever stream is in force.
func (c *revertibleConn) Read(b []byte) (int, error) {
	return c.current().Read(b)
}

// Write writes to whichever stream is in force.
func (c *revertibleConn) Write(b []byte) (int, error) {
	return c.current().Write(b)
}

var _ io.ReadWriter = (*revertibleConn)(nil)
