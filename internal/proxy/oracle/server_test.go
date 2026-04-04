package oracle

import (
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

func TestOracleServer_StartsAndAcceptsConnections(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, config.QueryStorageConfig{}, config.OracleDumpConfig{}, slog.Default())
	go func() { _ = srv.Start(":0") }()
	defer func() { _ = srv.Shutdown(t.Context()) }()

	// Wait for listener to be ready
	require.Eventually(t, func() bool { return srv.Addr() != nil }, time.Second, 10*time.Millisecond)

	conn, err := net.Dial("tcp", srv.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
}

func TestOracleServer_GracefulShutdown(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, config.QueryStorageConfig{}, config.OracleDumpConfig{}, slog.Default())
	go func() { _ = srv.Start(":0") }()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, time.Second, 10*time.Millisecond)

	conn, err := net.Dial("tcp", srv.Addr().String())
	require.NoError(t, err)

	// Close the client connection first so the session goroutine doesn't block
	_ = conn.Close()

	// Give the session goroutine time to see the close
	time.Sleep(50 * time.Millisecond)

	err = srv.Shutdown(t.Context())
	assert.NoError(t, err)
}

func TestOracleServer_ConcurrentConnections(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, config.QueryStorageConfig{}, config.OracleDumpConfig{}, slog.Default())
	go func() { _ = srv.Start(":0") }()
	defer func() { _ = srv.Shutdown(t.Context()) }()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, time.Second, 10*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", srv.Addr().String())
			if err == nil {
				_ = conn.Close()
			}
		}()
	}
	wg.Wait()
}
