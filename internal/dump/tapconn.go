package dump

import (
	"net"
)

// TapConn wraps a net.Conn and captures all read/written bytes to a Writer.
// Reads are tagged with one direction, writes with the other.
type TapConn struct {
	net.Conn
	writer   *Writer
	readDir  byte // Direction for Read operations
	writeDir byte // Direction for Write operations
}

// NewTapConn creates a connection wrapper that captures traffic to a dump Writer.
func NewTapConn(conn net.Conn, w *Writer, readDir, writeDir byte) *TapConn {
	return &TapConn{
		Conn:     conn,
		writer:   w,
		readDir:  readDir,
		writeDir: writeDir,
	}
}

// Read reads from the underlying connection and records the data.
func (t *TapConn) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		_ = t.writer.WritePacket(t.readDir, b[:n])
	}

	return n, err
}

// Write writes to the underlying connection and records the data.
func (t *TapConn) Write(b []byte) (int, error) {
	n, err := t.Conn.Write(b)
	if n > 0 {
		_ = t.writer.WritePacket(t.writeDir, b[:n])
	}

	return n, err
}
