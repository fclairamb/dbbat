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

// TestUpstreamToClient_ByteLimitHoldsRatherThanCuttingIn drives the real
// upstream→client TNS relay: a grant with a tiny byte cap trips while a reply is
// streaming, and the relay must write **nothing** into that reply.
//
// This is the whole of the mid-reply fix, seen from the response leg. dbbat cuts
// in at a TNS packet boundary while a fetch reply is a TTC message stream whose
// messages straddle packets, so an OER written here lands inside a half-
// delivered row batch — measured against Oracle 23ai Free, dbbat wrote one
// well-formed ORA-00028 with the right call number and neither ojdbc nor go-ora
// reported it. So the violation is armed and held, and the client's next call is
// what it is delivered on (TestHeldRefusalEndsTheCallTheClientIsNextParkedOn).
func TestUpstreamToClient_ByteLimitHoldsRatherThanCuttingIn(t *testing.T) {
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

	// Simulated upstream server: the tail of one reply, then end of stream.
	// Byte 2 (the TTC function code) is an unhandled value so the response
	// interceptor is a no-op — we only care that bytes flow. The cap is crossed
	// on the first packet, so every packet after it is relayed under a held
	// refusal, which is the bounded overshoot the design accepts.
	const replyPackets = 20

	go func() {
		payload := make([]byte, 40)
		payload[2] = 0x99

		pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: payload}

		for range replyPackets {
			if err := writeTNSPacket(upstreamTestEnd, pkt); err != nil {
				return
			}
		}

		_ = upstreamTestEnd.Close()
	}()

	// Simulated client: drain TNS packets until the relay is done and the pipe
	// closes, keeping them so the test can assert dbbat wrote none of its own.
	pktCh := make(chan []*TNSPacket, 1)

	go func() {
		var got []*TNSPacket

		for {
			pkt, err := readTNSPacket(clientTestEnd)
			if err != nil {
				pktCh <- got

				return
			}

			got = append(got, pkt)
		}
	}()

	// The relay ends on the upstream's own EOF, not on the violation: a held
	// refusal keeps relaying so the client can finish the reply it is parked in.
	require.NoError(t, s.upstreamToClient())

	_ = upstreamProxyEnd.Close()
	_ = clientProxyEnd.Close()
	_ = clientTestEnd.Close()

	held := s.heldRefusalNow()
	require.NotNil(t, held, "the violation must be armed for the client's next call")
	require.ErrorIs(t, held.err, shared.ErrByteQuotaExceeded)

	require.NotNil(t, s.tracker.pendingQuery,
		"the query is completed when the refusal is delivered, not when it is armed — "+
			"completing it here would charge the grant for bytes still being relayed")

	select {
	case got := <-pktCh:
		require.Len(t, got, replyPackets,
			"every packet of the reply must reach the client, and nothing else")

		for i, pkt := range got {
			require.Equalf(t, byte(0x99), pkt.Payload[2],
				"packet %d is not one the upstream sent: dbbat wrote a frame into a reply in progress", i)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for client packets")
	}
}

// TestHeldRefusalEndsTheCallTheClientIsNextParkedOn pins half of the decision
// recorded in docs/oracle.md, "An asynchronous refusal: which call number, and
// whether to send one at all".
//
// A limit violation caught on the response leg has no readable place to go: the
// client is in the middle of consuming a reply, and a frame written there is
// eaten as row bytes (measured — see the doc). The boundary the client *does*
// respect is its own next call, so the refusal waits for one and ends it, with
// that call's own sequence number rather than the stale one of the reply it was
// cut out of. That is also what a real Oracle does on ALTER SYSTEM KILL SESSION,
// and it is the path ojdbc's sequence-number check (see oerSummary.CallNumber)
// accepts.
func TestHeldRefusalEndsTheCallTheClientIsNextParkedOn(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
	})

	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}

	var from, to atomic.Int64

	to.Store(4096) // bytes the aborted query streamed before the cap bit

	s := newTestSession(grant)
	s.clientConn = clientProxyEnd
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	// The reply the violation was cut out of belonged to call 0x11 — the number
	// that must *not* end up on the frame, because the client has moved on.
	s.observeClientCallNumber(decodeHexString(t, "035e1100028021000101"))
	require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, 1)))
	require.True(t, s.hasPendingQuery())

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))

	// The client finished that reply and is now parked on a fresh call, 0x2a.
	// Sending this message is the boundary: it could not have been sent from
	// inside the previous reply.
	next := buildOALL8("SELECT id FROM emp", nil, 1)
	next[ttcOpSeqOffset] = 0x2a

	blocked := make(chan bool, 1)

	go func() {
		blocked <- s.interceptClientMessage(&TNSPacket{
			Type:    TNSPacketTypeData,
			Payload: append([]byte{0x00, 0x00}, next...),
		})
	}()

	require.NoError(t, clientTestEnd.SetReadDeadline(time.Now().Add(5*time.Second)))

	refusal, err := readTNSPacket(clientTestEnd)
	require.NoError(t, err)

	assert.True(t, <-blocked, "the refused call must not be forwarded upstream")

	body := refusal.Payload[ttcDataFlagsSize:]
	require.Equal(t, byte(TTCFuncOERR), body[0])

	info := decodeOERAt(body, 0)
	require.NotNil(t, info)
	assert.Equal(t, int(ORA00028), info.ErrorCode, "a held limit violation ends the next call with ORA-00028")
	assert.Contains(t, info.ErrorMessage, "session terminated", "the frame must carry the real reason")

	_, _, callNumber, ok := skipOERFixedFields(s.oer, body, 0)
	require.True(t, ok)
	assert.Equal(t, byte(0x2a), callNumber,
		"the refusal must end the call the client is parked on now, not the one it was cut out of")

	// Enforcement is unchanged by the hold: the aborted query is finalized with
	// the real reason and its streamed bytes are charged to the grant. With no
	// store wired, completeQuery skips the async persist but still bumps the
	// in-session counters — the value a real session persists and recomputes on
	// reconnect.
	assert.Nil(t, s.tracker.pendingQuery, "the aborted query must be completed when the refusal lands")
	assert.Positive(t, grant.BytesTransferred, "aborted query's streamed bytes must be attributed to the grant")

	select {
	case <-s.heldRefusalNow().done:
	default:
		t.Fatal("the handoff must be marked done so the watchdog stands down for good")
	}
}

// TestHeldRefusalMeetingAnUnnameableCallClosesInstead pins the fail-safe on the
// delivery side. dbbat can only end a call it can name; stamping a frame with
// the last number it saw ends a call the client is not parked on, which is the
// ORA-18745 / hang mode gateUnnameableFrame exists to avoid. So an unnameable
// next call gets the same answer that path gives: no frame, both sockets
// dropped.
func TestHeldRefusalMeetingAnUnnameableCallClosesInstead(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})

	client, answered := recordingPipe(t)
	s.clientConn = client
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	require.True(t, s.holdRefusal(shared.ErrGrantRevoked))

	// A bare 0x11 frame: the message type is the piggyback one and its close
	// list does not walk, so clientCallNumber declines to name it.
	assert.True(t, s.interceptClientMessage(&TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildOFETCH(5, 100)...),
	}), "the frame must not be forwarded under a held refusal")

	assert.Empty(t, answered(), "dbbat must not stamp a frame with a call number it could not read")
}

// TestHeldRefusalStandsTheWatchdogDownUntilItsGrace pins the seam that makes the
// hold possible at all: LimitGuard.Watch fires its hook once and returns, while
// the violation stays true for as long as the refusal is held. Without the wait,
// the watchdog would drop both sockets a poll interval after the quota was
// crossed and the client would meet the same ORA-03113 the hold exists to
// replace.
func TestHeldRefusalStandsTheWatchdogDownUntilItsGrace(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
	})

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.clientConn = clientProxyEnd

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))

	returned := make(chan struct{})

	go func() {
		s.onLimitViolation(shared.ErrByteQuotaExceeded)
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("the watchdog tore the session down on top of a refusal the client leg was about to deliver")
	case <-time.After(250 * time.Millisecond):
	}

	// The client leg answers: the watchdog has nothing left to do.
	s.finishRefusalHandoff(s.heldRefusalNow())

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never returned after the handoff completed")
	}

	assertOracleConnStillOpen(t, clientTestEnd)
}

// TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking is the other end
// of that stand-down: a handoff nobody collects must not keep an over-quota
// session alive. Past the grace the watchdog does what it always did — drops
// both sockets — and the statement is still recorded with the real reason.
func TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
	})

	grant := &store.Grant{ExpiresAt: time.Now().Add(time.Hour), Definition: &store.GrantDefinition{}}

	var from, to atomic.Int64

	to.Store(4096)

	s := newTestSession(grant)
	s.clientConn = clientProxyEnd
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    &trackedCursor{sql: "SELECT * FROM big"},
		startTime: time.Now(),
	}

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))
	backdateHeldRefusal(s)

	s.onLimitViolation(shared.ErrByteQuotaExceeded)

	assertOraclePeerClosed(t, clientTestEnd, "client conn")
	assert.Nil(t, s.tracker.pendingQuery, "the abandoned query must still be finalized")
	assert.Positive(t, grant.BytesTransferred, "its streamed bytes must still be charged to the grant")
}

// TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed pins the second
// fail-safe: the hold's cost is meant to be the tail of one fetch batch, so a
// reply that never reaches a boundary must not stream past the quota forever.
//
// The bound is exercised by backdating the held refusal's byte mark rather than
// by moving 8 MiB through a pipe — the arithmetic is the thing under test.
func TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{ExpiresAt: time.Now().Add(time.Hour), Definition: &store.GrantDefinition{}}

	var from, to atomic.Int64

	to.Store(4096)

	s := newTestSession(grant)
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.guard = shared.NewLimitGuard(grant, &from, &to)
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    &trackedCursor{sql: "SELECT * FROM big"},
		startTime: time.Now(),
	}

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))
	require.NoError(t, s.enforceMidStreamLimits(), "inside the bound the relay keeps going")

	s.refusalMu.Lock()
	s.held.atBytes -= refusalHoldMaxBytes + 1
	s.refusalMu.Unlock()

	require.ErrorIs(t, s.enforceMidStreamLimits(), shared.ErrByteQuotaExceeded,
		"past the bound the relay ends the session rather than keep streaming")
	assert.Nil(t, s.tracker.pendingQuery, "the abandoned query must still be finalized")
	assert.Positive(t, grant.BytesTransferred, "its streamed bytes must still be charged to the grant")
}

// backdateHeldRefusal makes the armed refusal's grace look already spent, so the
// fallback can be exercised without a 30s test.
func backdateHeldRefusal(s *session) {
	s.refusalMu.Lock()
	defer s.refusalMu.Unlock()

	s.held.armedAt = s.held.armedAt.Add(-refusalHandoffGrace - time.Second)
}

// assertOracleConnStillOpen is the inverse of assertOraclePeerClosed: a read
// that times out proves the conn was neither closed nor written to.
func assertOracleConnStillOpen(t *testing.T, c net.Conn) {
	t.Helper()

	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))

	buf := make([]byte, 1)

	_, err := c.Read(buf)
	if err == nil {
		t.Fatal("the conn received bytes, want it left alone")
	}

	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return
	}

	t.Fatalf("read err = %v, want a timeout (the conn was torn down)", err)
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
