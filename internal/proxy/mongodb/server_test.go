package mongodb

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// TestServerStartAndShutdown exercises the accept loop against a concurrent
// Shutdown. Its real value is under `-race`: the per-connection wg.Add used to
// be able to take the WaitGroup counter up from zero while Shutdown was already
// inside wg.Wait, which the detector flags and which could let Wait return
// before the connection it should have waited for was registered.
func TestServerStartAndShutdown(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.MongoConfig{TLS: config.TLSConfig{Disable: true}}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Nil(t, srv.Addr(), "no listener before Start")

	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Start("127.0.0.1:0")
	}()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}
