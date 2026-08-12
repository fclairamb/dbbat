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
	"github.com/stretchr/testify/assert"
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
	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}
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
	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}
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

	expired := newTestSession(&store.Grant{
		ExpiresAt:  time.Now().Add(-time.Minute),
		Definition: &store.GrantDefinition{},
	})
	require.ErrorIs(t, expired.checkQuotas(), shared.ErrGrantExpired)

	live := newTestSession(&store.Grant{
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})
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
	grant := &store.Grant{ExpiresAt: time.Now().Add(time.Hour), Definition: &store.GrantDefinition{MaxBytesTransferred: &maxBytes}}

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

// TestMidStreamRefusalEndsTheCallTheClientIsWaitingOn pins half of the decision
// recorded in docs/oracle.md, "An asynchronous refusal: which call number, and
// whether to send one at all".
//
// A limit violation caught on the *response* leg looks asynchronous — the
// client's call was forwarded upstream long ago and dbbat is cutting into its
// reply — but it is not: the check is gated on hasPendingQuery(), which is only
// true before the call's end-of-call OER has reached the client, so the client
// is still parked in the receive for the very op observeClientCallNumber
// recorded. Stamping that number is therefore right for the same reason it is
// right on the client leg, and ojdbc 26.1's sequence-number check (see
// oerSummary.CallNumber) passes.
func TestMidStreamRefusalEndsTheCallTheClientIsWaitingOn(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
		_ = upstreamProxyEnd.Close()
		_ = upstreamTestEnd.Close()
	})

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}
	h := reg.Register(grant.UID)

	var from, to atomic.Int64

	s := newTestSession(grant)
	s.clientConn = clientProxyEnd
	s.upstreamConn = upstreamProxyEnd
	s.revocation = h
	s.guard = shared.NewLimitGuard(grant, &from, &to).WithRevocation(h.Flag())
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	// The client's execute carries TTC sequence 0x11. It has been forwarded
	// upstream, so its reply is what the relay below is streaming back.
	s.observeClientCallNumber(decodeHexString(t, "035e1100028021000101"))
	require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, 1)))
	require.True(t, s.hasPendingQuery())

	// Revoked before the next relayed packet, so the post-forward guard check
	// trips on the first one.
	reg.Revoke(grant.UID)

	go func() { _ = s.upstreamToClient() }()

	// One packet of the reply. Byte 0 of the TTC payload is an unhandled
	// function code, so the response interceptor is a no-op and the pending
	// query survives to arm the guard check.
	go func() {
		_, _ = upstreamTestEnd.Write(encodeV315DataPacket([]byte{0x00, 0x00, 0x99}))
	}()

	require.NoError(t, clientTestEnd.SetReadDeadline(time.Now().Add(5*time.Second)))

	relayed, err := readTNSPacket(clientTestEnd)
	require.NoError(t, err)
	require.Equal(t, TNSPacketTypeData, relayed.Type)

	refusal, err := readTNSPacket(clientTestEnd)
	require.NoError(t, err)

	body := refusal.Payload[ttcDataFlagsSize:]
	require.Equal(t, byte(TTCFuncOERR), body[0])

	info := decodeOERAt(body, 0)
	require.NotNil(t, info)
	assert.Equal(t, int(ORA00028), info.ErrorCode, "a mid-reply limit violation ends the call with ORA-00028")

	_, _, callNumber, ok := skipOERFixedFields(s.oer, body, 0)
	require.True(t, ok)
	assert.Equal(t, byte(0x11), callNumber,
		"the mid-reply refusal must end the call the client is still waiting on, not zero")
}

// TestIdleLimitViolationSendsNoOER pins the other half: the limit watchdog is
// the one refusal path that can fire with the client idle between calls, and it
// must write no OER at all.
//
// TTC has no unsolicited server message. A frame written to an idle socket is
// consumed as the answer to the client's *next* request and carries, by
// construction, the previous call's number — which is exactly the mismatch
// ojdbc 26.1 reports as ORA-18745 wrapping the real code. Force-closing both
// conns surfaces as a plain I/O error instead, which is what a real Oracle's
// DISCONNECT SESSION produces. See onLimitViolation.
func TestIdleLimitViolationSendsNoOER(t *testing.T) {
	t.Parallel()

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}
	h := reg.Register(grant.UID)

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, _ := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
		_ = upstreamProxyEnd.Close()
	})

	var from, to atomic.Int64

	s := newTestSession(grant)
	s.clientConn = clientProxyEnd
	s.upstreamConn = upstreamProxyEnd
	s.revocation = h
	s.guard = shared.NewLimitGuard(grant, &from, &to).WithRevocation(h.Flag())
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	// The client ran a statement earlier and is now idle between calls: the
	// session still remembers call 0x11, and nothing may be stamped with it.
	s.observeClientCallNumber(decodeHexString(t, "035e1100028021000101"))

	go s.guard.Watch(context.Background(), 5*time.Millisecond, s.onLimitViolation)

	reg.Revoke(grant.UID)

	// assertOraclePeerClosed fails the test if the read returns any byte, which
	// is what makes this an assertion about the *absence* of an ORA-00028 frame
	// and not merely about the teardown.
	assertOraclePeerClosed(t, clientTestEnd, "client conn")
}
