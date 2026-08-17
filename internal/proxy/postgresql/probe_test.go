package postgresql

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// probeLogRecorder captures every record the proxy logs together with its
// level, which is the whole point: a bare TCP health check must leave a DEBUG
// trail and nothing louder.
type probeLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func newProbeLogRecorder() *probeLogRecorder { return &probeLogRecorder{} }

func (h *probeLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *probeLogRecorder) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, record.Clone())

	return nil
}

func (h *probeLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *probeLogRecorder) WithGroup(string) slog.Handler      { return h }

// reset drops what has been captured so far — used to ignore the listener's own
// startup lines, which are not what the test is about.
func (h *probeLogRecorder) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = nil
}

// noisy renders every record at or above INFO, so a failure names the line that
// broke the rule instead of just counting it.
func (h *probeLogRecorder) noisy() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []string

	for _, record := range h.records {
		if record.Level >= slog.LevelInfo {
			out = append(out, fmt.Sprintf("%s: %s", record.Level, record.Message))
		}
	}

	return out
}

// has reports whether a record with exactly this level and message was seen.
func (h *probeLogRecorder) has(level slog.Level, message string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, record := range h.records {
		if record.Level == level && record.Message == message {
			return true
		}
	}

	return false
}

// TestBareTCPProbeIsQuiet dials the listener and closes without sending a byte
// — exactly what a load balancer's TCP health check does — and asserts the
// proxy treats it as an expected outcome: a DEBUG line, nothing louder.
func TestBareTCPProbeIsQuiet(t *testing.T) {
	t.Parallel()

	logs := newProbeLogRecorder()

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.PGConfig{TLS: config.TLSConfig{Disable: true}}, slog.New(logs))
	require.NoError(t, err)

	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Start("127.0.0.1:0")
	}()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	logs.reset()

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		return logs.has(slog.LevelDebug, "Client disconnected before startup")
	}, 5*time.Second, 10*time.Millisecond, "the probe should stay observable at DEBUG")

	assert.Empty(t, logs.noisy(), "a bare TCP probe must not log above DEBUG")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}

// TestTruncatedStartupStaysAnError is the other half of the rule: a client that
// sends a partial startup packet and then hangs up is a real failure and must
// keep its ERROR line. Demoting it too would hide truncated clients.
func TestTruncatedStartupStaysAnError(t *testing.T) {
	t.Parallel()

	logs := newProbeLogRecorder()

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil,
		config.PGConfig{TLS: config.TLSConfig{Disable: true}}, slog.New(logs))
	require.NoError(t, err)

	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Start("127.0.0.1:0")
	}()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	logs.reset()

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)

	// Three bytes of an eight-byte startup header, then a hang-up.
	_, err = conn.Write([]byte{0x00, 0x00, 0x00})
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		return logs.has(slog.LevelError, "Session error")
	}, 5*time.Second, 10*time.Millisecond, "a truncated startup packet must stay an error")

	assert.False(t, logs.has(slog.LevelDebug, "Client disconnected before startup"),
		"a client that sent bytes is not a silent disconnect")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}
