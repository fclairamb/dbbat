package mongodb

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// wireHeaderOnly renders the 16-byte wire header declaring a total message
// length of n — the bytes an unauthenticated scanner sends when it wants an
// allocation without paying for the body.
func wireHeaderOnly(n int32) []byte {
	hdr := make([]byte, headerLen)
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(n))
	binary.LittleEndian.PutUint32(hdr[4:8], 1)      // requestID
	binary.LittleEndian.PutUint32(hdr[8:12], 0)     // responseTo
	binary.LittleEndian.PutUint32(hdr[12:16], 2013) // OP_MSG

	return hdr
}

// TestReadMessage_RejectsOversizedLength pins that a declared length past the
// wire maximum is refused on the header alone, before any body allocation.
func TestReadMessage_RejectsOversizedLength(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		declare int32
	}{
		{"one byte over the wire maximum", maxWireMessageSize + 1},
		{"two gigabytes", 0x7fffffff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, err := readMessage(bytes.NewReader(wireHeaderOnly(tc.declare)))
			require.Nil(t, m)
			require.ErrorIs(t, err, ErrMessageTooLarge)
		})
	}
}

// TestReadMessage_RejectsShortLength pins the lower bound: a length that
// cannot even cover the header it arrived in.
func TestReadMessage_RejectsShortLength(t *testing.T) {
	t.Parallel()

	for _, declare := range []int32{0, headerLen - 1, -1} {
		m, err := readMessage(bytes.NewReader(wireHeaderOnly(declare)))
		require.Nil(t, m)
		require.ErrorIs(t, err, ErrShortMessage)
	}
}

// TestReadMessage_DoesNotPreallocateDeclaredLength is the amplification guard.
// A message may legally declare up to 48 MB, so the size check alone does not
// stop 16 bytes of traffic from sizing a 48 MB buffer. The body is read in
// bounded chunks instead, so a header that declares the maximum and then
// delivers nothing must fail with a truncated-read error while having grown
// the buffer by no more than one chunk.
func TestReadMessage_DoesNotPreallocateDeclaredLength(t *testing.T) {
	t.Parallel()

	r := &countingReader{Reader: bytes.NewReader(wireHeaderOnly(maxWireMessageSize))}

	m, err := readMessage(r)
	require.Nil(t, m)
	require.ErrorIs(t, err, io.EOF)

	// The reader was asked for at most one chunk beyond the header, not the
	// whole declared 48 MB.
	require.LessOrEqual(t, r.maxRequested, frameReadChunk)
}

// TestReadMessage_ReadsCompleteBody pins that a well-formed message still
// round-trips through the chunked read, including one larger than a chunk.
func TestReadMessage_ReadsCompleteBody(t *testing.T) {
	t.Parallel()

	for _, bodyLen := range []int{4, frameReadChunk, frameReadChunk*2 + 7} {
		total := headerLen + bodyLen
		raw := wireHeaderOnly(int32(total))

		body := make([]byte, bodyLen)
		for i := range body {
			body[i] = byte(i)
		}

		raw = append(raw, body...)

		m, err := readMessage(bytes.NewReader(raw))
		require.NoError(t, err)
		require.Equal(t, int32(total), m.length)
		require.Equal(t, body, m.body)
		require.Equal(t, raw, m.raw)
	}
}

// countingReader records the largest single Read request it saw, which is what
// reveals whether the caller sized its buffer from the declared length.
type countingReader struct {
	io.Reader

	maxRequested int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) > c.maxRequested {
		c.maxRequested = len(p)
	}

	return c.Reader.Read(p)
}
