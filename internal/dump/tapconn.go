package dump

import (
	"io"
	"net"
)

// tapSkip disables recording for one leg of a TapConn. It is not a wire
// direction and never reaches the Writer — DirClientToServer and
// DirServerToClient are the only two values a capture holds.
const tapSkip byte = 0xFF

// TapConn wraps a net.Conn and captures read and/or written bytes to a Writer.
// Reads are tagged with one direction, writes with the other; either leg may be
// disabled (see NewWriteTapConn) when the proxy already records that direction
// itself and a second recording point would duplicate every frame.
type TapConn struct {
	net.Conn
	writer   *Writer
	readDir  byte // Direction for Read operations, or tapSkip
	writeDir byte // Direction for Write operations, or tapSkip
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

// NewWriteTapConn creates a wrapper that records only what is *written* to the
// connection, leaving the read side untouched.
//
// This is what a proxy wants when its own reader reassembles protocol messages
// before recording them — a read tap there would capture the socket's arbitrary
// chunk boundaries instead of the messages, and would double-record everything
// the reader already dumps. The write side has no such split: one message is one
// Write, so tapping it both preserves the framing and makes it impossible for a
// synthesized frame (an error the proxy answers itself) to reach the client
// without landing in the capture.
func NewWriteTapConn(conn net.Conn, w *Writer, writeDir byte) *TapConn {
	return &TapConn{
		Conn:     conn,
		writer:   w,
		readDir:  tapSkip,
		writeDir: writeDir,
	}
}

// Read reads from the underlying connection and records the data.
func (t *TapConn) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 && t.readDir != tapSkip {
		_ = t.writer.WritePacket(t.readDir, b[:n])
	}

	return n, err
}

// Write writes to the underlying connection and records the data.
func (t *TapConn) Write(b []byte) (int, error) {
	n, err := t.Conn.Write(b)
	if n > 0 && t.writeDir != tapSkip {
		_ = t.writer.WritePacket(t.writeDir, b[:n])
	}

	return n, err
}

// TapReader wraps an io.Reader and records what is read as one direction.
//
// It exists for a proxy whose inbound path is a *buffered* reader built before
// the capture is opened: wrapping the socket instead would strand whatever the
// buffer already holds (bytes a client pipelined behind its startup packet),
// and rebuilding the buffer would discard them outright. Tapping the reader
// itself keeps the buffer and puts the recording point on the only path the
// post-capture reads take.
type TapReader struct {
	r      io.Reader
	writer *Writer
	dir    byte
}

// NewTapReader records everything read through r under direction dir.
func NewTapReader(r io.Reader, w *Writer, dir byte) *TapReader {
	return &TapReader{r: r, writer: w, dir: dir}
}

// Read reads from the underlying reader and records the data.
func (t *TapReader) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if n > 0 {
		_ = t.writer.WritePacket(t.dir, b[:n])
	}

	return n, err
}
