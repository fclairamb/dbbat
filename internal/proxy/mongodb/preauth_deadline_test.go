package mongodb

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/config"
)

// TestPreAuthDeadlineClosesSilentClient opens a connection, sends nothing, and
// checks the proxy hangs up once the (shortened) pre-auth window has passed.
// Without the deadline the session waits on setupTransport forever and the
// client's own read deadline fires first.
func TestPreAuthDeadlineClosesSilentClient(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.MongoConfig{TLS: config.TLSConfig{Disable: true}}, slog.New(slog.DiscardHandler))
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

// TestPreAuthDeadlineIsIdleBoundForMonitors is the MongoDB-specific positive
// control: a driver's monitoring connection never authenticates and polls
// hello for its whole life, so the window is re-armed per message. A client
// that keeps talking stays connected well past one window; once it falls
// silent it is closed.
func TestPreAuthDeadlineIsIdleBoundForMonitors(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.MongoConfig{TLS: config.TLSConfig{Disable: true}}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	const window = 300 * time.Millisecond

	srv.preAuth.Set(window)

	errCh := make(chan error, 1)

	go func() { errCh <- srv.Start("127.0.0.1:0") }()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	start := time.Now()

	for i := int32(1); time.Since(start) < 4*window; i++ {
		hello, err := buildOpMsgReply(i, 0, bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}})
		require.NoError(t, err)

		_, err = conn.Write(hello)
		require.NoError(t, err)

		reply, err := readMessage(conn)
		require.NoError(t, err, "a polling monitor must not be cut off")
		require.Equal(t, i, reply.responseTo)

		time.Sleep(window / 3)
	}

	_, err = io.Copy(io.Discard, conn)
	require.NoError(t, err, "a silent connection must be closed by the proxy")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}
