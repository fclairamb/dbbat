package oracle

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// panicOnReadConn is a socket that blows up the moment the relay reads it —
// standing in for any panic on the read path (framing, a decoder, the dump
// writer) that is not already wrapped in a recover of its own.
type panicOnReadConn struct {
	net.Conn
}

func (*panicOnReadConn) Read([]byte) (int, error) { panic("read blew up") }

func (*panicOnReadConn) Close() error { return nil }

// TestRelayPanicEndsTheSessionNotTheProcess is the assertion the test binary
// makes for free: an unrecovered panic on *any* goroutine kills the whole
// process, so a run that reaches the bottom of this function is a run in which
// the relay's recover fired. Go's testing package cannot report that failure —
// there would be nothing left to report it.
//
// What it pins beyond mere survival is the half that keeps the session from
// leaking instead: the error has to arrive on errChan. proxyMessages blocks on
// `<-errChan` forever if a relay dies without sending, and a session parked
// there never runs its cleanup, never closes its connection record and never
// flushes its dump.
//
// The goroutine below is wired exactly as proxyMessages wires the upstream leg.
func TestRelayPanicEndsTheSessionNotTheProcess(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()

	defer func() {
		_ = clientProxyEnd.Close()
		_ = clientTestEnd.Close()
	}()

	var from, to atomic.Int64

	s := newTestSession(&store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})
	s.clientConn = clientProxyEnd
	s.upstreamConn = &panicOnReadConn{}
	s.bytesFromClient = &from
	s.bytesToClient = &to

	errChan := make(chan error, 2)

	go func() {
		errChan <- shared.RunRelay(s.ctx, s.logger, relayNameUpstreamToClient, s.upstreamToClient)
	}()

	select {
	case err := <-errChan:
		require.ErrorIs(t, err, shared.ErrRelayPanic,
			"the panic must come back as an error, not as a dead process: "+
				"the session tears down through the same channel an I/O error uses")
		assert.Contains(t, err.Error(), relayNameUpstreamToClient,
			"the error names the leg that died")
	case <-time.After(5 * time.Second):
		t.Fatal("the relay panicked without reporting: proxyMessages would block here forever")
	}
}

// TestClientRelayPanicReportsToo pins the same for the other direction, because
// the two legs are wired separately and a fix applied to one is not a fix.
func TestClientRelayPanicReportsToo(t *testing.T) {
	t.Parallel()

	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	defer func() {
		_ = upstreamProxyEnd.Close()
		_ = upstreamTestEnd.Close()
	}()

	var from, to atomic.Int64

	s := newTestSession(&store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})
	s.clientConn = &panicOnReadConn{}
	s.upstreamConn = upstreamProxyEnd
	s.bytesFromClient = &from
	s.bytesToClient = &to

	errChan := make(chan error, 2)

	go func() {
		errChan <- shared.RunRelay(s.ctx, s.logger, relayNameClientToUpstream, s.clientToUpstream)
	}()

	select {
	case err := <-errChan:
		require.ErrorIs(t, err, shared.ErrRelayPanic)
	case <-time.After(5 * time.Second):
		t.Fatal("the client relay panicked without reporting")
	}
}

// TestPreAuthPumpPanicUnblocksStopPump covers the pre-auth relay, which is the
// sharpest-edged of the three Oracle relay goroutines.
//
// pumpDone is buffered 1 and stopPump *blocks* reading it, at every exit of the
// pre-auth loop. So a pump that panicked without yielding a value would not
// merely leak the session the way a post-auth leg would: it would hang the auth
// handover outright, with the client waiting on a proxy waiting on a goroutine
// that no longer exists. This mirrors relayPreAuthNegotiation's wiring — keep
// the send here in step with the one there.
func TestPreAuthPumpPanicUnblocksStopPump(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{
		UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	})
	s.clientConn = &panicOnReadConn{}

	pumpDone := make(chan error, 1)

	go func() {
		pumpDone <- shared.RunRelay(s.ctx, s.logger, relayNamePreAuthPump, func() error {
			return s.pumpPreAuthUpstream(&panicOnReadConn{})
		})
	}()

	select {
	case err := <-pumpDone:
		require.ErrorIs(t, err, shared.ErrRelayPanic)
	case <-time.After(5 * time.Second):
		t.Fatal("stopPump would block here forever: the pre-auth pump panicked without reporting")
	}
}
