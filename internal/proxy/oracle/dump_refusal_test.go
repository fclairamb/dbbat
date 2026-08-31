package oracle

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

// dumpFixture is a session with a capture attached and a drained client socket,
// built the way run() builds one: startDumpIfConfigured is what installs the
// server→client recording point, so a test that wires the writer by hand would
// verify nothing about the path a real session takes.
type dumpFixture struct {
	s   *session
	dir string
	uid uuid.UUID
}

func newDumpFixture(t *testing.T) *dumpFixture {
	t.Helper()

	dir := t.TempDir()
	client, proxyEnd := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = proxyEnd.Close()
	})

	// net.Pipe is unbuffered: without a reader every write to the proxy end
	// blocks forever.
	go func() { _, _ = io.Copy(io.Discard, client) }()

	uid := uuid.New()
	s := &session{
		clientConn:    proxyEnd,
		ctx:           context.Background(),
		logger:        testLogger(),
		tracker:       newOracleQueryTracker(),
		connectionUID: uid,
		serviceName:   "TESTDB",
		dumpConfig:    config.DumpConfig{Dir: dir, MaxSize: 10 << 20},
		oer:           defaultOERShape(),
	}

	s.startDumpIfConfigured("10.0.0.1:1521")
	require.NotNil(t, s.dump, "capture should have been created")

	return &dumpFixture{s: s, dir: dir, uid: uid}
}

// packets closes the capture and reads every frame back out of it.
func (f *dumpFixture) packets(t *testing.T) []dump.Packet {
	t.Helper()

	require.NoError(t, f.s.dump.Close())

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

// TestDumpCapturesSynthesizedRefusal pins the whole point of the write tap: an
// OER dbbat writes itself — the frame every refusal rides on — has to be in the
// capture. It used to go straight to the socket, so a capture of a refused
// statement showed the statement and then silence.
func TestDumpCapturesSynthesizedRefusal(t *testing.T) {
	t.Parallel()

	f := newDumpFixture(t)

	require.NoError(t, f.s.sendOracleError(shared.ErrReadOnlyViolation))

	pkts := f.packets(t)
	require.Len(t, pkts, 1, "the refusal is exactly one frame")
	assert.Equal(t, dump.DirServerToClient, pkts[0].Direction)

	tns, err := parseTNSFromDumpPacket(pkts[0].Data)
	require.NoError(t, err, "the captured frame is a TNS packet")
	assert.Equal(t, TNSPacketTypeData, tns.Type)
	assert.Contains(t, string(pkts[0].Data), "ORA-01031", "captured frame carries the ORA code")
}

// TestDumpCapturesStatementNotFullyReadRefusal covers the refusal the fragment
// reassembly work added: it is answered through the same writeTTCError, and it
// is exactly the event an investigator opens a capture to see.
func TestDumpCapturesStatementNotFullyReadRefusal(t *testing.T) {
	t.Parallel()

	f := newDumpFixture(t)

	require.NoError(t, f.s.sendOracleError(shared.ErrStatementNotFullyRead))

	pkts := f.packets(t)
	require.Len(t, pkts, 1)
	assert.Equal(t, dump.DirServerToClient, pkts[0].Direction)
	assert.Contains(t, string(pkts[0].Data), "ORA-01031")
}

// TestDumpCapturesMidStreamLimitRefusal covers the other synthesized frame: the
// ORA-00028 a held mid-reply limit refusal delivers.
func TestDumpCapturesMidStreamLimitRefusal(t *testing.T) {
	t.Parallel()

	f := newDumpFixture(t)

	require.NoError(t, f.s.writeTTCError(int(ORA00028), "session terminated: quota exceeded"))

	pkts := f.packets(t)
	require.Len(t, pkts, 1)
	assert.Equal(t, dump.DirServerToClient, pkts[0].Direction)
	assert.Contains(t, string(pkts[0].Data), "ORA-00028")
}

// TestDumpRecordsRelayedFrameOnce guards the other half of the change: moving
// the recording point onto the socket must not leave the old explicit call
// behind. A duplicated frame in a forensic capture is its own bug.
func TestDumpRecordsRelayedFrameOnce(t *testing.T) {
	t.Parallel()

	f := newDumpFixture(t)

	relayed := &TNSPacket{Type: TNSPacketTypeData, Payload: []byte("upstream reply payload")}
	require.NoError(t, writeTNSPacket(f.s.clientConn, relayed))
	require.NoError(t, f.s.writeTTCError(1031, "refused"))

	pkts := f.packets(t)
	require.Len(t, pkts, 2, "one relayed frame + one refusal, each recorded once")
	assert.Contains(t, string(pkts[0].Data), "upstream reply payload")
	assert.Contains(t, string(pkts[1].Data), "ORA-01031")

	for i, pkt := range pkts {
		assert.Equal(t, dump.DirServerToClient, pkt.Direction, "packet %d", i)
	}
}

// TestDumpTapPreservesByteAccounting checks the tap did not displace the
// counting/watched conns it wraps: a session whose byte quota stopped counting
// once captures were enabled would be a far worse bug than the one being fixed.
func TestDumpTapPreservesByteAccounting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	client, proxyEnd := net.Pipe()

	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	go func() { _, _ = io.Copy(io.Discard, client) }()

	s := newSession(proxyEnd, nil, nil, testLogger(), context.Background(), nil, config.QueryStorageConfig{},
		config.DumpConfig{Dir: dir, MaxSize: 10 << 20}, nil)
	s.connectionUID = uuid.New()
	s.startDumpIfConfigured("10.0.0.1:1521")
	require.NotNil(t, s.dump)

	require.NoError(t, s.writeTTCError(1031, "refused"))
	require.NoError(t, s.dump.Close())

	assert.Positive(t, s.bytesToClient.Load(), "the refusal is still charged to the session")
	assert.NotNil(t, s.watched, "client-gone detection survives the tap")

	// And the counting conn is still underneath the tap rather than replaced.
	tap, ok := s.clientConn.(*dump.TapConn)
	require.True(t, ok, "client conn is wrapped by the write tap")
	_, ok = tap.Conn.(*shared.CountingConn)
	assert.True(t, ok, "the counting conn is still underneath")
}

// TestDumpDisabledLeavesClientConnUnwrapped keeps the no-capture path free of
// the tap entirely — the common configuration.
func TestDumpDisabledLeavesClientConnUnwrapped(t *testing.T) {
	t.Parallel()

	client, proxyEnd := net.Pipe()

	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	s := &session{
		clientConn:    proxyEnd,
		ctx:           context.Background(),
		logger:        testLogger(),
		connectionUID: uuid.New(),
		dumpConfig:    config.DumpConfig{},
	}

	s.startDumpIfConfigured("10.0.0.1:1521")

	assert.Nil(t, s.dump)
	assert.Equal(t, proxyEnd, s.clientConn)
}

// TestDumpSkippedWithoutConnectionUID pins why the AUTH-phase refusal cannot be
// captured: the capture is named after the connection row, which does not exist
// until authentication has succeeded.
func TestDumpSkippedWithoutConnectionUID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	client, proxyEnd := net.Pipe()

	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	s := &session{
		clientConn: proxyEnd,
		ctx:        context.Background(),
		logger:     testLogger(),
		dumpConfig: config.DumpConfig{Dir: dir, MaxSize: 10 << 20},
	}

	s.startDumpIfConfigured("10.0.0.1:1521")

	assert.Nil(t, s.dump)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no capture file without a connection UID")
}
