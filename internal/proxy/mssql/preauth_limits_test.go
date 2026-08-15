package mssql

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// measuringReader records the largest single Read request the parser made,
// which is the buffer it sized from the wire.
type measuringReader struct {
	io.Reader

	maxRequested int
}

func (m *measuringReader) Read(p []byte) (int, error) {
	if len(p) > m.maxRequested {
		m.maxRequested = len(p)
	}

	return m.Reader.Read(p)
}

// TestReadPacketLengthIsStructurallyBounded pins the property that keeps the
// TDS listener out of the pre-auth memory-bomb class. PRELOGIN and LOGIN7 are
// both parsed before the peer has authenticated, and readPacket sizes its
// payload buffer straight from the header — but the length field is a uint16,
// so the largest buffer eight attacker-chosen bytes can name is 64 KiB, no
// matter what the bits say. Anything bigger has to be paid for packet by
// packet, which is what ErrMessageTooLarge then bounds.
//
// This is the test that would fail if the length field were ever widened
// without a cap being added alongside it.
func TestReadPacketLengthIsStructurallyBounded(t *testing.T) {
	t.Parallel()

	// A PRELOGIN header declaring the largest length the field can hold, with
	// no payload behind it: the peer names a size and sends nothing.
	hdr := []byte{packetTypePrelogin, statusEOM, 0xff, 0xff, 0x00, 0x00, 0x01, 0x00}

	reader := &measuringReader{Reader: &sliceReader{data: hdr}}
	rw := newPacketRW(struct {
		io.Reader
		io.Writer
	}{Reader: reader, Writer: io.Discard})

	_, body, err := rw.ReadMessage()
	require.ErrorIs(t, err, ErrShortPayload)
	require.Nil(t, body)
	require.LessOrEqual(t, reader.maxRequested, 1<<16)
}

// TestReadPacketRejectsALengthBelowItsOwnHeader pins the lower bound: a length
// under the 8 bytes it includes would underflow the payload size into a
// negative make(), which panics.
func TestReadPacketRejectsALengthBelowItsOwnHeader(t *testing.T) {
	t.Parallel()

	for _, declared := range []byte{0x00, 0x01, packetHeaderSize - 1} {
		hdr := []byte{packetTypePrelogin, statusEOM, 0x00, declared, 0x00, 0x00, 0x01, 0x00}

		rw := newPacketRW(struct {
			io.Reader
			io.Writer
		}{Reader: &sliceReader{data: hdr}, Writer: io.Discard})

		_, _, err := rw.ReadMessage()
		require.ErrorIs(t, err, ErrBadPacketLength)
	}
}

// sliceReader hands out a fixed byte slice once, then reports EOF.
type sliceReader struct {
	data []byte
	pos  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}

	n := copy(p, s.data[s.pos:])
	s.pos += n

	return n, nil
}
