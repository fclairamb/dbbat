package shared

import (
	"net"
	"sync/atomic"
)

// CountingConn wraps a net.Conn and atomically tracks the number of bytes
// read from and written to it. The two counters live outside the wrapper so
// a session can share them across multiple wrapped conns (e.g. client and
// upstream): writes to one direction on one wrapper match reads from the
// same direction on the other.
//
// Total() is safe to call concurrently with Read/Write — useful for taking
// per-query snapshots while the proxy is mid-stream.
type CountingConn struct {
	net.Conn
	bytesRead    *atomic.Int64
	bytesWritten *atomic.Int64
}

// NewCountingConn wraps conn so Read accumulates into bytesRead and Write
// accumulates into bytesWritten. Either counter may be nil to disable that
// direction (rare; the typical caller passes both).
func NewCountingConn(conn net.Conn, bytesRead, bytesWritten *atomic.Int64) *CountingConn {
	return &CountingConn{
		Conn:         conn,
		bytesRead:    bytesRead,
		bytesWritten: bytesWritten,
	}
}

// Read implements net.Conn. Successful byte counts are added to the read
// counter even when the call returns an error (n > 0 with err is a valid
// outcome on a closing conn — those bytes did cross the wire).
func (c *CountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.bytesRead != nil {
		c.bytesRead.Add(int64(n))
	}

	return n, err
}

// Write implements net.Conn with the same byte-counting semantics as Read.
func (c *CountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && c.bytesWritten != nil {
		c.bytesWritten.Add(int64(n))
	}

	return n, err
}
