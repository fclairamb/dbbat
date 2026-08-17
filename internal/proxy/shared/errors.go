package shared

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// ErrClientDisconnectedBeforeStartup marks a connection that was opened and
// closed again without the client sending a single byte — the shape of a plain
// TCP health check (an NLB, a Kubernetes readiness probe, a port scanner).
//
// It exists because every protocol front-end reads before it can do anything,
// so a bare open/close surfaces as a read failure deep inside the startup
// handshake and used to be logged as a session error. On Stonal's deployment
// that was ~22 ERROR lines every 3 minutes on the PostgreSQL listener alone,
// all from the load balancer, which is exactly how a reader learns to ignore
// the ERROR level.
//
// A listener that receives this must log at DEBUG (with the remote address, so
// it is still there when someone is chasing a connectivity problem) rather than
// ERROR/WARN/INFO, and must not emit the paired "session ended" line: the
// session never began.
var ErrClientDisconnectedBeforeStartup = errors.New("client disconnected before sending any data")

// IsClientHangup reports whether err is the transport saying the peer is gone:
// a clean FIN (io.EOF), a reset (ECONNRESET — some load balancers close their
// probe that way), or a socket already closed underneath us (net.ErrClosed,
// which is what an in-flight read sees during shutdown).
//
// Deliberately *not* io.ErrUnexpectedEOF: that is a peer which stopped halfway
// through a frame, i.e. a truncated client, which is a real failure and must
// stay loud.
func IsClientHangup(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed)
}

// ClientDisconnectedBeforeStartup classifies a failure raised while the startup
// handshake was still reading. It returns err wrapped in
// ErrClientDisconnectedBeforeStartup when — and only when — the client hung up
// having sent *zero* bytes; otherwise it returns nil and the caller keeps its
// own error.
//
// The zero-byte condition is the whole point and is checked against the
// session's own client-side read counter (the CountingConn every protocol
// already installs). An EOF *mid*-startup-packet leaves that counter non-zero
// and stays an error: a truncated client is a genuine problem, "opened the
// socket and said nothing" is not.
func ClientDisconnectedBeforeStartup(bytesFromClient int64, err error) error {
	if err == nil || bytesFromClient != 0 || !IsClientHangup(err) {
		return nil
	}

	return fmt.Errorf("%w: %w", ErrClientDisconnectedBeforeStartup, err)
}
