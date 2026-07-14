package oracle

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestCheckQuotas_Revoked asserts a revoked grant blocks the next command on a
// live Oracle connection.
func TestCheckQuotas_Revoked(t *testing.T) {
	t.Parallel()

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)}
	h := reg.Register(grant.UID)

	s := newTestSession(grant)
	s.revocation = h

	require.NoError(t, s.checkQuotas())

	reg.Revoke(grant.UID)

	require.ErrorIs(t, s.checkQuotas(), shared.ErrGrantRevoked)
}

// TestRevocation_DisconnectsLiveSession drives the registry → guard watchdog →
// onLimitViolation seam: revoking a grant while the watchdog runs force-closes
// both conns.
func TestRevocation_DisconnectsLiveSession(t *testing.T) {
	t.Parallel()

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)}
	h := reg.Register(grant.UID)

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	var from, to atomic.Int64

	s := &session{
		clientConn:      clientProxyEnd,
		upstreamConn:    upstreamProxyEnd,
		logger:          testLogger(),
		ctx:             context.Background(),
		grant:           grant,
		revocation:      h,
		guard:           shared.NewLimitGuard(grant, &from, &to).WithRevocation(h.Flag()),
		tracker:         newOracleQueryTracker(),
		bytesFromClient: &from,
		bytesToClient:   &to,
	}

	go s.guard.Watch(context.Background(), 5*time.Millisecond, s.onLimitViolation)

	reg.Revoke(grant.UID)

	assertOraclePeerClosed(t, clientTestEnd, "client conn")
	assertOraclePeerClosed(t, upstreamTestEnd, "upstream conn")
}

func assertOraclePeerClosed(t *testing.T, c net.Conn, name string) {
	t.Helper()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 1)

	_, err := c.Read(buf)
	if err == nil {
		t.Fatalf("%s: read succeeded, want the conn to be closed", name)
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return
	}

	t.Fatalf("%s: read err = %v, want EOF/closed-pipe (conn not torn down in time)", name, err)
}

// TestCheckQuotas_Expiry asserts the between-commands expiry gap is closed: a
// command issued after the grant's ExpiresAt is rejected with ErrGrantExpired.
func TestCheckQuotas_Expiry(t *testing.T) {
	t.Parallel()

	expired := newTestSession(&store.Grant{ExpiresAt: time.Now().Add(-time.Minute)})
	require.ErrorIs(t, expired.checkQuotas(), shared.ErrGrantExpired)

	live := newTestSession(&store.Grant{ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, live.checkQuotas())
}

// TestUpstreamToClient_ByteLimitAbort drives the real upstream→client TNS relay:
// a grant with a tiny byte cap must cut a streaming result off mid-flight
// (upstreamToClient returns ErrByteQuotaExceeded after emitting a TTC error
// frame) rather than forwarding the whole result.
func TestUpstreamToClient_ByteLimitAbort(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	var from, to atomic.Int64

	counted := shared.NewCountingConn(clientProxyEnd, &from, &to)

	maxBytes := int64(200)
	grant := &store.Grant{
		MaxBytesTransferred: &maxBytes,
		ExpiresAt:           time.Now().Add(time.Hour),
	}

	s := &session{
		clientConn:      counted,
		upstreamConn:    upstreamProxyEnd,
		logger:          testLogger(),
		ctx:             context.Background(),
		grant:           grant,
		guard:           shared.NewLimitGuard(grant, &from, &to),
		tracker:         newOracleQueryTracker(),
		bytesFromClient: &from,
		bytesToClient:   &to,
	}
	// A query must be in flight for the mid-stream check to fire.
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    &trackedCursor{sql: "SELECT * FROM big"},
		startTime: time.Now(),
	}

	// Simulated upstream server: stream TNS Data packets until the pipe errors.
	// Byte 2 (the TTC function code) is an unhandled value so the response
	// interceptor is a no-op — we only care that bytes flow.
	go func() {
		payload := make([]byte, 40)
		payload[2] = 0x99

		pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: payload}

		for i := 0; i < 100000; i++ {
			if err := writeTNSPacket(upstreamTestEnd, pkt); err != nil {
				return
			}
		}
	}()

	// Simulated client: drain TNS packets until EOF, counting them (the
	// forwarded rows plus the terminating TTC error frame).
	pktCh := make(chan int, 1)

	go func() {
		n := 0

		for {
			if _, err := readTNSPacket(clientTestEnd); err != nil {
				pktCh <- n

				return
			}

			n++
		}
	}()

	relayErr := s.upstreamToClient()

	_ = upstreamTestEnd.Close()
	_ = upstreamProxyEnd.Close()
	_ = clientTestEnd.Close()
	_ = clientProxyEnd.Close()

	require.ErrorIs(t, relayErr, shared.ErrByteQuotaExceeded)

	// The aborted query's streamed-so-far bytes must be attributed to the grant.
	// With no store wired, completeQuery skips the async persist but still bumps
	// the in-session grant counters — the value a real session persists and
	// recomputes on reconnect.
	require.Positive(t, grant.BytesTransferred, "aborted query's streamed bytes must be attributed to the grant")
	require.Nil(t, s.tracker.pendingQuery, "completeQuery must clear the pending query on abort")

	select {
	case n := <-pktCh:
		require.Positive(t, n, "client should have received forwarded packets plus the TTC error frame")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for client packets")
	}
}
