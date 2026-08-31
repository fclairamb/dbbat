package dump

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTapWriter opens a capture and returns it with a reader over it.
func newTapWriter(t *testing.T) (*Writer, func() []Packet) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tap"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, 0)
	require.NoError(t, err)

	return w, func() []Packet {
		require.NoError(t, w.Close())

		r, err := OpenReader(path)
		require.NoError(t, err)

		defer func() { _ = r.Close() }()

		var out []Packet

		for {
			pkt, err := r.ReadPacket()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				require.NoError(t, err)
			}

			out = append(out, *pkt)
		}

		return out
	}
}

// pipePair returns a connected pair with the far end drained, so a write to the
// near end never blocks on an unbuffered net.Pipe.
func pipePair(t *testing.T) net.Conn {
	t.Helper()

	far, near := net.Pipe()

	t.Cleanup(func() {
		_ = far.Close()
		_ = near.Close()
	})

	go func() { _, _ = io.Copy(io.Discard, far) }()

	return near
}

func TestTapConn_RecordsBothDirections(t *testing.T) {
	t.Parallel()

	w, read := newTapWriter(t)
	tap := NewTapConn(pipePair(t), w, DirClientToServer, DirServerToClient)

	_, err := tap.Write([]byte("to the client"))
	require.NoError(t, err)

	pkts := read()
	require.Len(t, pkts, 1)
	assert.Equal(t, DirServerToClient, pkts[0].Direction)
	assert.Equal(t, []byte("to the client"), pkts[0].Data)
}

// A write-only tap is what a proxy installs when its own reader already records
// the inbound direction after reassembling protocol messages: a read tap there
// would both duplicate those frames and record socket chunk boundaries instead
// of messages.
func TestWriteTapConn_RecordsWritesOnly(t *testing.T) {
	t.Parallel()

	far, near := net.Pipe()

	defer func() { _ = far.Close() }()
	defer func() { _ = near.Close() }()

	w, read := newTapWriter(t)
	tap := NewWriteTapConn(near, w, DirServerToClient)

	go func() {
		_, _ = far.Write([]byte("from the client"))
		_, _ = io.Copy(io.Discard, far)
	}()

	buf := make([]byte, 64)
	n, err := tap.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "from the client", string(buf[:n]), "the read still reaches the caller")

	_, err = tap.Write([]byte("refusal frame"))
	require.NoError(t, err)

	pkts := read()
	require.Len(t, pkts, 1, "only the write is recorded")
	assert.Equal(t, DirServerToClient, pkts[0].Direction)
	assert.Equal(t, []byte("refusal frame"), pkts[0].Data)
}
