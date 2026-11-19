package api

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStartReturnsNilAfterShutdown pins the contract main relies on: Start's
// error means the listener failed, so a graceful Shutdown must not surface as
// one. It used to return http.ErrServerClosed, which main turned into
// os.Exit(1) halfway through every shutdown.
func TestStartReturnsNilAfterShutdown(t *testing.T) {
	t.Parallel()

	srv, _ := setupTestServer(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	errCh := make(chan error, 1)

	go func() { errCh <- srv.Start(addr) }()

	require.Eventually(t, func() bool {
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr != nil {
			return false
		}

		_ = conn.Close()

		return true
	}, 5*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}
