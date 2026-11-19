package mssql

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// TestPreAuthDeadlineClosesSilentClient opens a connection, sends nothing, and
// checks the proxy hangs up once the (shortened) pre-auth window has passed.
// Without the deadline the session waits on readPrelogin forever and the
// client's own read deadline fires first.
func TestPreAuthDeadlineClosesSilentClient(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

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

// TestPreAuthDeadlineSparesAuthenticatedIdleSession is the positive control:
// a session that logged in and then sits idle for longer than the window is
// not closed, because the deadline is lifted before the relay starts. The
// proof is the client's own short read timing out on a live socket rather
// than seeing the proxy's EOF.
func TestPreAuthDeadlineSparesAuthenticatedIdleSession(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	const window = 300 * time.Millisecond

	fixture.proxy.preAuth.Set(window)

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.Nil(t, outcome.Failure)
	require.True(t, outcome.Acked)

	time.Sleep(3 * window)

	require.NoError(t, client.conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))

	_, err := client.conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the authenticated session must still be open")
}
