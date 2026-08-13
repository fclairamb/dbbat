package oracle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
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

// TestHeldRefusalRecordsWhatTheHandoffCost guards the measurement the two
// fail-safe bounds are sized against.
//
// refusalHoldMaxBytes and refusalHandoffGrace are no longer reasoned numbers:
// docs/oracle.md carries per-client observations of `relayed_bytes_since` and
// `held_for_ms` taken off the delivered path by the integration suites. Those
// suites read the attributes back through countingHandler.intsFor, so an
// attribute that stopped being emitted — or was renamed — would turn every one
// of those measurements into an assertion on an empty slice. The compiler
// catches a rename of the constants; this catches the emission.
//
// It also pins the arithmetic itself, which nothing else does: the byte figure
// is the session's cumulative client-leg traffic *since* the violation, not
// since the session opened.
func TestHeldRefusalRecordsWhatTheHandoffCost(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}

	var from, to atomic.Int64

	// Where the session stood when the quota was crossed. Everything before this
	// belongs to the reply the client was already draining and must not be
	// counted against the hold.
	to.Store(4096)

	logs := newCountingHandler()

	client, _ := recordingPipe(t)

	s := newTestSession(grant)
	s.logger = slog.New(logs)
	s.clientConn = client
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))

	// The tail of the in-flight fetch batch, relayed while the refusal waited.
	const tailBytes = 1500

	to.Store(4096 + tailBytes)

	next := buildOALL8("SELECT id FROM emp", nil, 1)
	next[ttcOpSeqOffset] = 0x2a

	require.True(t, s.interceptClientMessage(&TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, next...),
	}))

	require.Equal(t, 1, logs.count(logMsgRefusalDelivered), "the refusal must have been delivered")

	assert.Equal(t, []int64{tailBytes},
		logs.intsFor(logMsgRefusalDelivered, logAttrRelayedBytesSince),
		"the delivered record must carry the bytes relayed *past the violation*, which is the "+
			"quantity refusalHoldMaxBytes bounds and the integration suites read back")

	waits := logs.intsFor(logMsgRefusalDelivered, logAttrHeldForMillis)
	require.Len(t, waits, 1,
		"and how long the hold lasted, which is the quantity refusalHandoffGrace bounds")
	assert.GreaterOrEqual(t, waits[0], int64(0))
	assert.Less(t, waits[0], refusalHandoffGrace.Milliseconds(),
		"a hold answered in-process cannot have taken the whole grace")
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
		Payload: []byte{0x00, 0x00, byte(TTCFuncOFETCH), 0x00, 0x05, 0x00, 0x00, 0x00, 0x64},
	}), "the frame must not be forwarded under a held refusal")

	assert.Empty(t, answered(), "dbbat must not stamp a frame with a call number it could not read")
}

// TestHeldRefusalBlocksAFrameItCannotRead pins the one place the intercept
// path's fail-open has to flip. Everything dbbat cannot read is normally
// forwarded — refusing a frame it could not identify protects nothing and
// strands the client — but under a held refusal the grant is exhausted and the
// session is already over, so forwarding would make an unreadable frame the
// single way a client message travels past an exhausted grant.
//
// It is answered as an unnameable call, because that is what it is: no frame,
// both sockets dropped.
//
// The fixture is the smallest packet clientToUpstream will actually hand over —
// it gates at ttcDataFlagsSize+1 bytes — so this is a shape that can really
// arrive, rather than one only a direct caller can produce. Its TTC payload is a
// single unknown byte: too short for clientCallNumber to name, which is what
// routes it to the same answer.
func TestHeldRefusalBlocksAFrameItCannotRead(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})

	client, answered := recordingPipe(t)
	s.clientConn = client
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	unreadable := &TNSPacket{Type: TNSPacketTypeData, Payload: []byte{0x00, 0x00, 0x99}}

	assert.False(t, s.interceptClientMessage(unreadable),
		"with no refusal held, an unreadable frame must still travel — that fail-open is load-bearing")

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))

	assert.True(t, s.interceptClientMessage(unreadable),
		"under a held refusal it must not be forwarded: the grant is exhausted")
	assert.Empty(t, answered(), "and no frame may be stamped with a call number dbbat never read")
}

// TestHeldRefusalTearsDownEvenWhenTheFrameWriteBlowsUp pins the exit that is not
// one of the four: writing the refusal panics.
//
// That path is only reachable through a panicking write, which is why the
// teardown in answerHeldRefusal is deferred rather than sequential — a
// sequential one would skip the recording and leave both sockets open, and the
// panic would then escape to the client relay goroutine. Since shared.RunRelay
// that costs the session rather than the process — better, but still the
// session. So this asserts the two halves that keep the exits at four: the
// statement is still finalized with the real reason, and nothing escapes.
func TestHeldRefusalTearsDownEvenWhenTheFrameWriteBlowsUp(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}

	var from, to atomic.Int64

	to.Store(4096)

	logs := newCountingHandler()

	s := newTestSession(grant)
	s.logger = slog.New(logs)
	s.clientConn = &panicOnWriteConn{}
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.oer = defaultOERShape()
	s.oer.tailLearned = true
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    &trackedCursor{sql: "SELECT * FROM big"},
		startTime: time.Now(),
	}

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))

	next := buildOALL8("SELECT id FROM emp", nil, 1)
	next[ttcOpSeqOffset] = 0x2a

	assert.True(t, s.interceptClientMessage(&TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, next...),
	}), "a panicking write must not turn into a forwarded frame")

	// Without this the assertions below would pass on a write that simply
	// succeeded, and the test would prove nothing about the panic-shaped exit.
	require.Positive(t, logs.count("recovered from panic intercepting client message"),
		"the write must actually have panicked and been recovered")

	assert.Nil(t, s.tracker.pendingQuery, "the aborted query must still be finalized")
	assert.Positive(t, grant.BytesTransferred, "its streamed bytes must still be charged to the grant")

	select {
	case <-s.heldRefusalNow().done:
	default:
		t.Fatal("the handoff must still be marked done, or the watchdog waits out its whole grace")
	}
}

// panicOnWriteConn is the frame write failing in the worst way available: not an
// error return, which writeTTCError already handles, but a panic mid-frame.
type panicOnWriteConn struct {
	net.Conn
}

func (*panicOnWriteConn) Write([]byte) (int, error) { panic("write blew up") }

func (*panicOnWriteConn) Close() error { return nil }

// TestHeldRefusalIsAnsweredExactlyOnce pins the one-frame invariant the whole
// measurement rests on. The client leg keeps reading until its socket really
// closes, so a pipelined second message must not draw a second end-of-call OER
// — a frame for a call nobody is parked on is precisely the unsolicited write
// onLimitViolation refuses to make.
func TestHeldRefusalIsAnsweredExactlyOnce(t *testing.T) {
	t.Parallel()

	client := &countingWriteConn{}

	s := newTestSession(&store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})
	s.clientConn = client
	s.oer = defaultOERShape()
	s.oer.tailLearned = true

	require.True(t, s.holdRefusal(shared.ErrByteQuotaExceeded))

	next := buildOALL8("SELECT id FROM emp", nil, 1)
	next[ttcOpSeqOffset] = 0x2a
	pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0x00, 0x00}, next...)}

	require.True(t, s.interceptClientMessage(pkt))
	require.Equal(t, 1, client.count(), "the held refusal must be answered")

	// A second message — pipelined, or read before the socket close landed —
	// and an unreadable one, which reaches the same answer by the other route.
	assert.True(t, s.interceptClientMessage(pkt), "a second call must not be forwarded either")
	assert.True(t, s.interceptClientMessage(&TNSPacket{Type: TNSPacketTypeData, Payload: []byte{0x00}}))

	assert.Equal(t, 1, client.count(),
		"exactly one end-of-call OER may go out: a second answers a call nobody is parked on")
}

// countingWriteConn counts the frames written to it and declines to close, so a
// test can watch what a session does *after* it has torn itself down. Only
// Write and Close are ever reached; the embedded nil net.Conn would panic on
// anything else, which is the point — it would be a change worth noticing.
type countingWriteConn struct {
	net.Conn

	mu     sync.Mutex
	writes int
}

func (c *countingWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writes++

	return len(p), nil
}

func (c *countingWriteConn) Close() error { return nil }

func (c *countingWriteConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.writes
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

	s.held.armedAt = s.held.armedAt.Add(-s.refusalGrace() - time.Second)
}

// TestUpstreamToClient_StopsRelayingPastTheOvershootBound is the same fail-safe
// as TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed, reached the way
// a live session reaches it: by the **relay** running out of bound, rather than
// by a direct call to enforceMidStreamLimits with a mark moved by hand.
//
// The distinction is the reason this test exists. The arithmetic test pins the
// subtraction; it cannot show that upstreamToClient consults the bound after
// every forwarded packet, nor that a non-nil return there actually ends the
// relay. The upstream here is one that never finishes its reply — the exact
// failure the bound is a fail-safe for, and one no real database produces, which
// is why the bound had never fired end to end.
//
// The bound is shrunk rather than the pipe fed 8 MiB: at ~48 bytes a packet that
// would be ~180k round trips through a synchronous net.Pipe, and it would make
// the loop count, not the relay, the thing being measured.
func TestUpstreamToClient_StopsRelayingPastTheOvershootBound(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
		_ = upstreamProxyEnd.Close()
		_ = upstreamTestEnd.Close()
	})

	var from, to atomic.Int64

	counted := shared.NewCountingConn(clientProxyEnd, &from, &to)

	maxBytes := int64(200)
	grant := &store.Grant{
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{MaxBytesTransferred: &maxBytes},
	}

	logs := newCountingHandler()

	const (
		overshootBound   = 4096
		replyPayloadSize = 40
		replyPacketSize  = tnsHeaderSize + replyPayloadSize
	)

	s := newTestSession(grant)
	s.logger = slog.New(logs)
	s.clientConn = counted
	s.upstreamConn = upstreamProxyEnd
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.guard = shared.NewLimitGuard(grant, &from, &to)
	s.refusalHoldBytes = overshootBound
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    &trackedCursor{sql: "SELECT * FROM never_ends"},
		startTime: time.Now(),
	}

	// An upstream whose reply has no end: it keeps writing row-shaped Data
	// packets until the relay gives up and the pipe breaks under it. Byte 2 is
	// an unhandled TTC function code, so the response interceptor is a no-op and
	// only the byte flow matters.
	go func() {
		payload := make([]byte, replyPayloadSize)
		payload[2] = 0x99

		pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: payload}

		for {
			if err := writeTNSPacket(upstreamTestEnd, pkt); err != nil {
				return
			}
		}
	}()

	// A client that keeps reading, so nothing stalls on pipe back-pressure and
	// the relay is bounded by the fail-safe rather than by a blocked write.
	relayed := make(chan int, 1)

	go func() {
		count := 0

		for {
			if _, err := readTNSPacket(clientTestEnd); err != nil {
				relayed <- count

				return
			}

			count++
		}
	}()

	err := s.upstreamToClient()
	require.ErrorIs(t, err, shared.ErrByteQuotaExceeded,
		"past the bound the relay must end the session rather than keep streaming a reply with no end")

	held := s.heldRefusalNow()
	require.NotNil(t, held)

	overshoot := s.cumulativeClientBytes() - held.atBytes
	t.Logf("relayed %d bytes past the violation under a %d-byte bound", overshoot, overshootBound)

	assert.Greater(t, overshoot, int64(overshootBound),
		"the relay must have run past the bound before giving up — otherwise it stopped for some other reason")
	assert.LessOrEqual(t, overshoot, int64(overshootBound+replyPacketSize),
		"and it must give up within one packet of the bound; anything more is an unbounded relay")

	assert.Positive(t, logs.count(logMsgRefusalHandoffAbandoned),
		"the abandonment must be logged as itself, not as a watchdog teardown")
	assert.Nil(t, s.tracker.pendingQuery, "the abandoned query must still be finalized")
	assert.Positive(t, grant.BytesTransferred, "its streamed bytes must still be charged to the grant")

	select {
	case <-held.done:
	default:
		t.Fatal("the handoff must be marked done, or the watchdog waits out its whole grace on top of it")
	}

	_ = clientProxyEnd.Close()
	_ = clientTestEnd.Close()

	select {
	case count := <-relayed:
		assert.Positive(t, count, "the client must have received the reply it was parked in")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the client reader to finish")
	}
}

// TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut is the grace fail-safe
// reached through the path a stuck client really takes: the violation is armed
// by enforceMidStreamLimits, the real LimitGuard.Watch fires onLimitViolation,
// the client never speaks, and the grace expires on its own.
//
// TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking pins the same
// teardown, but reaches it by backdating armedAt and calling onLimitViolation
// directly — which cannot show that the watchdog gets there at all, nor that
// awaitRefusalHandoff's timer is armed off the grace rather than off something
// that never fires. A millisecond grace is what makes the real wait testable.
func TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	t.Cleanup(func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
		_ = upstreamProxyEnd.Close()
		_ = upstreamTestEnd.Close()
	})

	var from, to atomic.Int64

	// The quota is already spent: this is the state the relay would be in when
	// it noticed the crossing mid-reply.
	maxBytes := int64(200)
	to.Store(4096)

	grant := &store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{MaxBytesTransferred: &maxBytes},
	}

	recorder := newRecordingCompletionStore()

	s := newTestSession(grant)
	s.clientConn = clientProxyEnd
	s.upstreamConn = upstreamProxyEnd
	s.completionStore = recorder
	s.bytesFromClient = &from
	s.bytesToClient = &to
	s.guard = shared.NewLimitGuard(grant, &from, &to)
	s.refusalHoldGrace = 20 * time.Millisecond
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    &trackedCursor{sql: "SELECT * FROM big"},
		startTime: time.Now(),
	}

	// Armed the way the relay arms it, not by calling holdRefusal: this is the
	// production path from "the guard says the quota is gone" to "a refusal is
	// waiting for the client's next call".
	require.NoError(t, s.enforceMidStreamLimits(), "arming the hold must not end the relay")
	require.NotNil(t, s.heldRefusalNow(), "the violation must be held for the client's next call")

	go s.guard.Watch(context.Background(), 5*time.Millisecond, s.onLimitViolation)

	// The client never speaks again, so the handoff never lands and the grace is
	// what ends the session — with the fallback the hold exists to avoid only
	// when it is working: both sockets dropped, an ORA-03113 that is meant.
	assertOraclePeerClosed(t, clientTestEnd, "client conn")
	assertOraclePeerClosed(t, upstreamTestEnd, "upstream conn")

	logged := recorder.awaitCreated(t)
	require.NotNil(t, logged.Error)
	assert.Equal(t, "aborted: "+shared.ErrByteQuotaExceeded.Error(), *logged.Error,
		"the statement must carry the real reason even when nobody was there to be told it")
	assert.Equal(t, "SELECT * FROM big", logged.SQLText)

	assert.Nil(t, s.tracker.pendingQuery, "the abandoned query must be finalized")
	assert.Positive(t, grant.BytesTransferred, "its streamed bytes must still be charged to the grant")
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
