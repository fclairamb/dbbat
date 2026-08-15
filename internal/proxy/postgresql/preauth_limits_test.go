package postgresql

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// preAuthSession wires a Session over a fixed byte stream. Only the reader is
// touched by the two pre-auth frame parsers, so nothing else has to exist.
func preAuthSession(raw []byte) *Session {
	return &Session{
		clientReader: bufio.NewReader(bytes.NewReader(raw)),
		ctx:          context.Background(),
	}
}

// lengthPrefix renders a 4-byte big-endian PostgreSQL message length.
func lengthPrefix(n uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, n)

	return buf
}

// TestReceiveStartupMessage_RejectsOversizedLength is the regression test for
// the pre-auth memory bomb: four bytes on an unauthenticated socket used to
// size make([]byte, length) directly, so a scanner sending 0xffffffff and
// nothing else asked the runtime for ~4 GiB. The frame must be refused on the
// declared length alone, before any allocation and without reading a body that
// was never sent.
func TestReceiveStartupMessage_RejectsOversizedLength(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		declare uint32
	}{
		{"high bit set", 0xffffffff},
		{"two gigabytes", 0x7fffffff},
		{"one byte over the limit", maxStartupPacketLength + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Only the length prefix is on the wire. If the parser tried to
			// read the declared body it would block or EOF instead of
			// returning the size refusal.
			s := preAuthSession(lengthPrefix(tc.declare))

			msg, err := s.receiveStartupMessage()
			require.Nil(t, msg)
			require.ErrorIs(t, err, ErrStartupMessageTooLarge)
		})
	}
}

// TestReceiveStartupMessage_RejectsMalformedLength covers the lengths that
// cannot cover their own length field. Before the check, length 0 produced a
// zero-length buffer whose msgBuf[4:] slice panicked.
func TestReceiveStartupMessage_RejectsMalformedLength(t *testing.T) {
	t.Parallel()

	for _, declare := range []uint32{0, 1, 3} {
		s := preAuthSession(lengthPrefix(declare))

		msg, err := s.receiveStartupMessage()
		require.Nil(t, msg)
		require.ErrorIs(t, err, ErrMalformedMessageLength)
	}
}

// TestReceiveStartupMessage_AcceptsRealStartup pins the other side of the
// bound: a StartupMessage of the shape libpq actually sends still parses.
func TestReceiveStartupMessage_AcceptsRealStartup(t *testing.T) {
	t.Parallel()

	body := []byte("user\x00alice\x00database\x00demo\x00\x00")
	raw := append(lengthPrefix(uint32(4+4+len(body))), lengthPrefix(196608)...)
	raw = append(raw, body...)

	s := preAuthSession(raw)

	msg, err := s.receiveStartupMessage()
	require.NoError(t, err)
	require.NotNil(t, msg)
}

// TestReceivePasswordMessage_RejectsOversizedLength is the same bomb one frame
// later: the password message is still pre-auth, and its length was used
// unchecked as make([]byte, length-4).
func TestReceivePasswordMessage_RejectsOversizedLength(t *testing.T) {
	t.Parallel()

	raw := append([]byte{'p'}, lengthPrefix(0xffffffff)...)
	s := preAuthSession(raw)

	msg, err := s.receivePasswordMessage()
	require.Nil(t, msg)
	require.ErrorIs(t, err, ErrPasswordMessageTooLarge)
}

// TestReceivePasswordMessage_RejectsMalformedLength covers length <= 4, where
// make([]byte, length-4) was a panic (negative) or an empty slice whose
// dataBuf[:len-1] slice panicked.
func TestReceivePasswordMessage_RejectsMalformedLength(t *testing.T) {
	t.Parallel()

	for _, declare := range []uint32{0, 3, 4} {
		raw := append([]byte{'p'}, lengthPrefix(declare)...)
		s := preAuthSession(raw)

		msg, err := s.receivePasswordMessage()
		require.Nil(t, msg)
		require.ErrorIs(t, err, ErrMalformedMessageLength)
	}
}

// TestReceivePasswordMessage_AcceptsRealPassword pins that an ordinary
// password still round-trips under the cap.
func TestReceivePasswordMessage_AcceptsRealPassword(t *testing.T) {
	t.Parallel()

	secret := []byte("hunter2\x00")
	raw := append([]byte{'p'}, lengthPrefix(uint32(4+len(secret)))...)
	raw = append(raw, secret...)

	s := preAuthSession(raw)

	msg, err := s.receivePasswordMessage()
	require.NoError(t, err)
	require.Equal(t, "hunter2", msg.Password)
}
