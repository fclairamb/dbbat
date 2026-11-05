package shared

import (
	"net"
	"sync/atomic"
	"time"
)

// PreAuthTimeout bounds how long an unauthenticated client may hold a
// connection: from the accept to the moment its login is accepted. A client
// that opens a socket and says nothing — or dribbles a few bytes — would
// otherwise hold a goroutine, its buffers and a file descriptor forever, and
// enough of them exhaust the process's descriptors for every listener and the
// API. Stock PostgreSQL's authentication_timeout is the same idea (60s).
//
// It is a deadline on the client socket, so it covers writes too: a client
// that never reads the greeting cannot park the session on a full send
// buffer either.
const PreAuthTimeout = 30 * time.Second

// PreAuthTimer carries a listener's pre-authentication window. The zero value
// means PreAuthTimeout. Atomic because tests shorten it on a server that is
// already accepting.
type PreAuthTimer struct {
	timeout atomic.Int64
}

// Set overrides the window; d <= 0 restores PreAuthTimeout.
func (p *PreAuthTimer) Set(d time.Duration) {
	p.timeout.Store(int64(d))
}

// Timeout returns the window in force.
func (p *PreAuthTimer) Timeout() time.Duration {
	if d := time.Duration(p.timeout.Load()); d > 0 {
		return d
	}

	return PreAuthTimeout
}

// ArmPreAuthDeadline sets the pre-authentication deadline on conn, d from now
// (d <= 0 means PreAuthTimeout). Every conn wrapper dbbat installs
// (CountingConn, WatchedConn, *tls.Conn) delegates deadlines to the socket, so
// arming the outermost one bounds them all.
func ArmPreAuthDeadline(conn net.Conn, d time.Duration) error {
	if d <= 0 {
		d = PreAuthTimeout
	}

	return conn.SetDeadline(time.Now().Add(d))
}

// ClearPreAuthDeadline lifts the deadline once the client is authenticated: an
// established session may sit idle for as long as the client keeps it open.
func ClearPreAuthDeadline(conn net.Conn) error {
	return conn.SetDeadline(time.Time{})
}
