package oracle

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// TestPreAuthDeadlineClosesSilentClient opens a connection, sends nothing, and
// checks the proxy hangs up once the (shortened) pre-auth window has passed.
// Without the deadline the session waits on readConnectPacket forever and the
// client's own read deadline fires first.
func TestPreAuthDeadlineClosesSilentClient(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, slog.New(slog.DiscardHandler))

	srv.preAuth.Set(200 * time.Millisecond)

	errCh := make(chan error, 1)

	go func() { errCh <- srv.Start("127.0.0.1:0") }()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	start := time.Now()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, err = io.Copy(io.Discard, conn)
	require.NoError(t, err, "the proxy must close the connection, not leave it to the client's deadline")
	require.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}

// TestPreAuthRelayCapRefusesBeforeDialing fills the pre-auth relay semaphore
// and checks the next login is refused with ORA-12520 right after its TNS
// Connect — before dbbat would have opened an upstream connection for it.
func TestPreAuthRelayCapRefusesBeforeDialing(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, slog.New(slog.DiscardHandler))
	srv.preAuthRelays = make(chan struct{}, 1)
	srv.preAuthRelays <- struct{}{} // one login already in flight

	errCh := make(chan error, 1)

	go func() { errCh <- srv.Start("127.0.0.1:0") }()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	_, err = conn.Write(encodeTNSPacket(TNSPacketTypeConnect, buildTestTNSConnect("TESTDB")))
	require.NoError(t, err)

	pkt, err := readTNSPacket(conn)
	require.NoError(t, err)
	require.Equal(t, TNSPacketTypeRedirect, pkt.Type)
	require.Contains(t, string(pkt.Payload), "12520")

	require.Len(t, srv.preAuthRelays, 1, "a refused login must not take or free a slot")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}

func TestAcquirePreAuthRelayReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	s := &session{preAuthRelays: make(chan struct{}, 1)}

	release, ok := s.acquirePreAuthRelay()
	require.True(t, ok)

	_, ok = s.acquirePreAuthRelay()
	require.False(t, ok, "the only slot is taken")

	release()
	release() // the deferred call after the success-path one

	require.Empty(t, s.preAuthRelays)

	release2, ok := s.acquirePreAuthRelay()
	require.True(t, ok)
	release2()

	unbounded := &session{}
	release3, ok := unbounded.acquirePreAuthRelay()
	require.True(t, ok)
	release3()
}
