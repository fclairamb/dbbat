package mongodb

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// panicOnReadConn blows up the moment a pump reads it — standing in for any
// panic on the wire-decode path that is not already inside a recover of its own.
type panicOnReadConn struct {
	net.Conn
}

func (*panicOnReadConn) Read([]byte) (int, error) { panic("read blew up") }

func (*panicOnReadConn) Write(p []byte) (int, error) { return len(p), nil }

func (*panicOnReadConn) Close() error { return nil }

// TestRelayPanicEndsTheSessionNotTheProcess drives the *real* relay(), not a
// copy of its wiring, and that is the point of it.
//
// Two things could go wrong and only one of them is the obvious one. A relay
// goroutine that panics unrecovered kills the process — and with it this test
// binary, which is the assertion Go makes for free. But relay() also drains
// errCh a **second** time to wait the other pump out, so a panicking pump that
// was recovered without yielding a value would leave this function parked
// forever. That is the failure a later refactor would most plausibly
// reintroduce, by moving the send inside the recovered call
// (`go func() { if err := safe.RunRelay(…); err != nil { … } }()`), and the
// timeout below is what would catch it.
func TestRelayPanicEndsTheSessionNotTheProcess(t *testing.T) {
	t.Parallel()

	clientConn := &panicOnReadConn{}
	upstreamConn := &panicOnReadConn{}

	s := &Session{
		ctx:        context.Background(),
		logger:     slog.New(slog.DiscardHandler),
		clientConn: clientConn,
		reader:     bufio.NewReader(clientConn),
		upstream: &UpstreamConn{
			conn:   upstreamConn,
			reader: bufio.NewReader(upstreamConn),
		},
	}

	done := make(chan error, 1)

	go func() { done <- s.relay() }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay never returned: a pump panicked without yielding a value, " +
			"so the second <-errCh parked the session for good")
	}
}

// blockingConn parks in Read until Close, which is what a live socket does
// while its peer has nothing to say.
type blockingConn struct {
	net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newBlockingConn() *blockingConn {
	return &blockingConn{closed: make(chan struct{})}
}

func (c *blockingConn) Read([]byte) (int, error) {
	<-c.closed

	return 0, io.EOF
}

func (*blockingConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *blockingConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })

	return nil
}

// eofConn ends the client pump immediately, so relay() reaches closeUpstream
// while the upstream pump is still live.
type eofConn struct {
	net.Conn
}

func (*eofConn) Read([]byte) (int, error) { return 0, io.EOF }

func (*eofConn) Write(p []byte) (int, error) { return len(p), nil }

func (*eofConn) Close() error { return nil }

// TestRelayTeardownDoesNotRaceThePeerPump pins the shape that made relay()
// unsafe under -race: whichever pump returns first, relay() closes the upstream
// from *that* goroutine to unblock the other one — which at that instant is
// still reading s.upstream. closeUpstream() used to also nil the field, so the
// teardown wrote what the surviving pump was reading, with no ordering between
// them. Closing the connection is the whole job; clearing the pointer was a
// data race on production code paths (session.go's upstream→client pump and
// intercept.go's upstream write).
//
// Only meaningful under -race, and only probabilistically per iteration, hence
// the repeats.
func TestRelayTeardownDoesNotRaceThePeerPump(t *testing.T) {
	t.Parallel()

	for range 50 {
		clientConn := &eofConn{}
		upstreamConn := newBlockingConn()

		s := &Session{
			ctx:        context.Background(),
			logger:     slog.New(slog.DiscardHandler),
			clientConn: clientConn,
			reader:     bufio.NewReader(clientConn),
			upstream: &UpstreamConn{
				conn:   upstreamConn,
				reader: bufio.NewReader(upstreamConn),
			},
		}

		done := make(chan error, 1)

		go func() { done <- s.relay() }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("relay never returned: closeUpstream failed to unblock the peer pump")
		}
	}
}
