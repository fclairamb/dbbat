package shared

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreAuthTimerDefaultsAndOverride(t *testing.T) {
	t.Parallel()

	var p PreAuthTimer
	require.Equal(t, PreAuthTimeout, p.Timeout())

	p.Set(time.Second)
	require.Equal(t, time.Second, p.Timeout())

	p.Set(0)
	require.Equal(t, PreAuthTimeout, p.Timeout())
}

// An armed deadline fails a silent read; clearing it lets the conn idle.
func TestArmAndClearPreAuthDeadline(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	require.NoError(t, ArmPreAuthDeadline(server, 50*time.Millisecond))

	_, err := server.Read(make([]byte, 1))
	require.True(t, errors.Is(err, os.ErrDeadlineExceeded), "got %v", err)

	require.NoError(t, ArmPreAuthDeadline(server, 50*time.Millisecond))
	require.NoError(t, ClearPreAuthDeadline(server))

	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = client.Write([]byte{1})
	}()

	n, err := server.Read(make([]byte, 1))
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
