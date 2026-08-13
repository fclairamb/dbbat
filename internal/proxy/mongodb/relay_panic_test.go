package mongodb

import (
	"bufio"
	"context"
	"log/slog"
	"net"
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
