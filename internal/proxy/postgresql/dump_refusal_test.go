package postgresql

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

// pgDumpFixture is a post-auth session with a capture attached, wired by
// attachDumpTaps — the same call Run makes — so a test cannot pass against
// recording points a real session does not have.
type pgDumpFixture struct {
	s      *Session
	client net.Conn
	dir    string
	uid    uuid.UUID
}

func newPGDumpFixture(t *testing.T) *pgDumpFixture {
	t.Helper()

	dir := t.TempDir()
	client, proxyEnd := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = proxyEnd.Close()
	})

	uid := uuid.New()
	s := &Session{
		clientConn:    proxyEnd,
		clientReader:  bufio.NewReader(proxyEnd),
		logger:        slog.New(slog.DiscardHandler),
		ctx:           context.Background(),
		connectionUID: uid,
		dumpConfig:    config.DumpConfig{Dir: dir, MaxSize: 10 << 20},
		extendedState: &extendedQueryState{
			preparedStatements: make(map[string]*preparedStatement),
			portals:            make(map[string]*portalState),
		},
	}

	dw, err := dump.NewWriter(filepath.Join(dir, uid.String()+dump.FileExt), dump.Header{
		SessionID:  uid.String(),
		Protocol:   dump.ProtocolPostgreSQL,
		StartTime:  time.Now(),
		Connection: map[string]any{"database": "app"},
	}, s.dumpConfig.MaxSize)
	require.NoError(t, err)

	s.dumpWriter = dw
	s.attachDumpTaps(dw)

	return &pgDumpFixture{s: s, client: client, dir: dir, uid: uid}
}

// drainClient discards whatever the proxy writes, so an unbuffered net.Pipe
// write never blocks.
func (f *pgDumpFixture) drainClient() {
	go func() { _, _ = io.Copy(io.Discard, f.client) }()
}

func (f *pgDumpFixture) packets(t *testing.T) []dump.Packet {
	t.Helper()

	require.NoError(t, f.s.dumpWriter.Close())

	path := filepath.Join(f.dir, f.uid.String()+dump.FileExt)
	_, err := os.Stat(path)
	require.NoError(t, err, "capture file should exist at %s", path)

	r, err := dump.OpenReader(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	var out []dump.Packet

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

// serverToClient filters the capture down to the direction the synthesized
// frames travel in. Frame counts are asserted on this direction only: a write
// is one Encode and one Write, whereas the inbound side is recorded at whatever
// chunk boundaries the reader happens to produce.
func serverToClient(pkts []dump.Packet) []dump.Packet {
	var out []dump.Packet

	for _, pkt := range pkts {
		if pkt.Direction == dump.DirServerToClient {
			out = append(out, pkt)
		}
	}

	return out
}

// TestDumpCapturesSynthesizedQueryRefusal is the whole point: sendQueryError is
// the live path for every read_only / block_ddl / block_copy / quota refusal and
// it writes to the socket directly, so before the fix the capture of a refused
// query held the query and then nothing.
func TestDumpCapturesSynthesizedQueryRefusal(t *testing.T) {
	t.Parallel()

	f := newPGDumpFixture(t)
	f.drainClient()

	f.s.sendQueryError(shared.ErrReadOnlyViolation, false)

	out := serverToClient(f.packets(t))
	require.Len(t, out, 1, "the ErrorResponse is exactly one frame")
	assert.Equal(t, byte('E'), out[0].Data[0], "captured frame is an ErrorResponse")
	assert.Contains(t, string(out[0].Data), "write operations not permitted")
}

// The simple-query form owes a trailing ReadyForQuery, and it is a separate
// write to the same socket: both belong in the capture, each exactly once.
func TestDumpCapturesRefusalWithReadyForQuery(t *testing.T) {
	t.Parallel()

	f := newPGDumpFixture(t)
	f.drainClient()

	f.s.sendQueryError(shared.ErrReadOnlyViolation, true)

	out := serverToClient(f.packets(t))
	require.Len(t, out, 2, "ErrorResponse + ReadyForQuery, each recorded once")
	assert.Equal(t, byte('E'), out[0].Data[0])
	assert.Equal(t, byte('Z'), out[1].Data[0], "captured frame is a ReadyForQuery")
}

// errTestQuotaExceeded stands in for the guard's limit error.
var errTestQuotaExceeded = errors.New("grant byte quota exceeded")

// abortStream is the other direct writer: the mid-stream limit abort.
func TestDumpCapturesMidStreamAbort(t *testing.T) {
	t.Parallel()

	f := newPGDumpFixture(t)
	f.drainClient()

	f.s.abortStream(errTestQuotaExceeded)

	out := serverToClient(f.packets(t))
	require.Len(t, out, 2, "ErrorResponse + ReadyForQuery, each recorded once")
	assert.Equal(t, byte('E'), out[0].Data[0])
	assert.Contains(t, string(out[0].Data), "grant byte quota exceeded")
	assert.Contains(t, string(out[0].Data), "53400", "carries configuration_limit_exceeded")
	assert.Equal(t, byte('Z'), out[1].Data[0])
}

// TestDumpRecordsBackendWriteOnce guards the count: the recording point moved
// onto s.clientConn, which the pgproto3 backend also writes through. A frame
// recorded by both the backend wrapper and the conn would be a duplicate, and a
// duplicated frame in a forensic capture is its own bug.
func TestDumpRecordsBackendWriteOnce(t *testing.T) {
	t.Parallel()

	f := newPGDumpFixture(t)
	f.drainClient()

	f.s.clientBackend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	require.NoError(t, f.s.clientBackend.Flush())

	f.s.sendQueryError(shared.ErrReadOnlyViolation, false)

	out := serverToClient(f.packets(t))
	require.Len(t, out, 2, "one relayed frame + one synthesized frame, each once")
	assert.Equal(t, byte('Z'), out[0].Data[0])
	assert.Equal(t, byte('E'), out[1].Data[0])
}

// The inbound tap has to see the client's message even when the grant refuses
// it: a blocked query never reaches the upstream, so a capture that recorded at
// the forwarding point would hold neither the query nor its refusal.
func TestDumpCapturesClientQuery(t *testing.T) {
	t.Parallel()

	f := newPGDumpFixture(t)
	f.drainClient()

	go func() {
		buf, err := (&pgproto3.Query{String: "DELETE FROM secrets"}).Encode(nil)
		if err != nil {
			return
		}

		_, _ = f.client.Write(buf)
	}()

	msg, err := f.s.clientBackend.Receive()
	require.NoError(t, err)
	require.IsType(t, &pgproto3.Query{}, msg)

	var inbound []byte

	for _, pkt := range f.packets(t) {
		if pkt.Direction == dump.DirClientToServer {
			inbound = append(inbound, pkt.Data...)
		}
	}

	assert.Contains(t, string(inbound), "DELETE FROM secrets",
		"the client's query is in the capture, gating or not")
}

// The inbound tap must not swallow bytes the buffered reader already holds —
// the pipelining case that made rebuilding the reader the wrong shape.
func TestDumpTapPreservesBufferedClientBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	client, proxyEnd := net.Pipe()

	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	first, err := (&pgproto3.Query{String: "SELECT 1"}).Encode(nil)
	require.NoError(t, err)
	second, err := (&pgproto3.Query{String: "SELECT 2"}).Encode(nil)
	require.NoError(t, err)

	go func() { _, _ = client.Write(append(first, second...)) }()

	reader := bufio.NewReader(proxyEnd)

	// Fill the buffer with both messages before the capture exists, which is
	// what a client pipelining behind its startup packet does.
	_, err = reader.Peek(len(first) + len(second))
	require.NoError(t, err)

	uid := uuid.New()
	s := &Session{
		clientConn:    proxyEnd,
		clientReader:  reader,
		logger:        slog.New(slog.DiscardHandler),
		ctx:           context.Background(),
		connectionUID: uid,
		dumpConfig:    config.DumpConfig{Dir: dir, MaxSize: 10 << 20},
	}

	dw, err := dump.NewWriter(filepath.Join(dir, uid.String()+dump.FileExt), dump.Header{
		SessionID:  uid.String(),
		Protocol:   dump.ProtocolPostgreSQL,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, s.dumpConfig.MaxSize)
	require.NoError(t, err)

	s.dumpWriter = dw
	s.attachDumpTaps(dw)

	for _, want := range []string{"SELECT 1", "SELECT 2"} {
		msg, err := s.clientBackend.Receive()
		require.NoError(t, err, "buffered message %q survived the tap", want)

		query, ok := msg.(*pgproto3.Query)
		require.True(t, ok)
		assert.Equal(t, want, query.String)
	}

	require.NoError(t, dw.Close())
}
