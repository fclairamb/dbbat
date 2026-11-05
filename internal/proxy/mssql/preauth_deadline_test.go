package mssql

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
