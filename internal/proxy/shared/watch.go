package shared

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Client TCP keepalive settings. With no approval timeout, a hold ends only on
// approve, deny, or client disconnect — so "the client went away" has to be
// something the kernel actually tells us. A hard-killed client or a dead
// network path never sends a FIN, and without keepalive "until disconnect"
// silently means "forever", parking an upstream connection and any locks it
// holds indefinitely.
const (
	// ClientKeepAliveIdle is how long a connection may sit idle before the
	// first probe.
	ClientKeepAliveIdle = 30 * time.Second
	// ClientKeepAliveInterval is the gap between probes.
	ClientKeepAliveInterval = 10 * time.Second
	// ClientKeepAliveCount is how many unanswered probes declare the peer dead.
	ClientKeepAliveCount = 3
)

// EnableClientKeepAlive turns on TCP keepalive with dbbat's probe settings on
// the underlying TCP socket. Non-TCP conns (tests, unix sockets) are a no-op.
// Errors are returned for logging but are never fatal: a proxy that cannot set
// keepalive is degraded, not broken.
func EnableClientKeepAlive(conn net.Conn) error {
	tcp, ok := underlyingTCP(conn)
	if !ok {
		return nil
	}

	return tcp.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     ClientKeepAliveIdle,
		Interval: ClientKeepAliveInterval,
		Count:    ClientKeepAliveCount,
	})
}

// underlyingTCP unwraps the conn chain looking for a *net.TCPConn.
func underlyingTCP(conn net.Conn) (*net.TCPConn, bool) {
	for range 8 { // bounded: guards against a pathological wrapper cycle
		switch c := conn.(type) {
		case *net.TCPConn:
			return c, true
		case interface{ NetConn() net.Conn }: // *tls.Conn
			conn = c.NetConn()
		case interface{ Unwrap() net.Conn }: // CountingConn, WatchedConn
			conn = c.Unwrap()
		default:
			return nil, false
		}

		if conn == nil {
			return nil, false
		}
	}

	return nil, false
}

// WatchedConn is a net.Conn wrapper that can be "parked": while parked, a
// background goroutine keeps reading the socket so a client FIN is noticed
// immediately instead of sitting unread until the session resumes.
//
// This is load-bearing rather than decorative. During an approval hold the
// session goroutine is blocked on a human, so nothing is reading the client
// socket; without an active read-watch a disconnected client would leave the
// statement parked forever.
//
// Bytes a pipelining client sent while parked are *not* dropped and *not*
// interpreted mid-hold: they are queued and replayed, in stream order, the
// moment the session resumes reading.
//
// Placement matters: WatchedConn must sit *below* any TLS layer, so it parks
// on raw TLS records it never has to decrypt.
type WatchedConn struct {
	net.Conn

	mu      sync.Mutex
	pending []byte // bytes captured while parked, replayed first on resume
	parked  bool
	err     error // terminal read error observed by the watcher

	stopped chan struct{} // closed when the watcher goroutine has exited
}

// GoroutineNameParkWatcher is what a panic in the park read-watch is logged
// under.
const GoroutineNameParkWatcher = "approval hold client read-watch"

// NewWatchedConn wraps conn.
func NewWatchedConn(conn net.Conn) *WatchedConn {
	return &WatchedConn{Conn: conn}
}

// Unwrap exposes the wrapped conn so helpers like EnableClientKeepAlive can
// reach the TCP socket underneath.
func (w *WatchedConn) Unwrap() net.Conn { return w.Conn }

// Read serves any bytes captured while parked before touching the socket, so
// the byte stream the session sees is exactly the byte stream the client sent.
func (w *WatchedConn) Read(p []byte) (int, error) {
	w.mu.Lock()

	if len(w.pending) > 0 {
		n := copy(p, w.pending)
		w.pending = w.pending[n:]
		w.mu.Unlock()

		return n, nil
	}

	// A terminal error seen by the watcher is reported once the replay queue
	// is drained — never before, or we would lose queued statements.
	if w.err != nil {
		err := w.err
		w.mu.Unlock()

		return 0, err
	}

	parked := w.parked
	w.mu.Unlock()

	if parked {
		// The watcher owns the socket while parked; a concurrent read here
		// would interleave two readers on one stream.
		return 0, ErrConnParked
	}

	return w.Conn.Read(p)
}

// ErrConnParked is returned if something tries to read the connection while a
// hold owns it. It indicates a wiring bug, not a runtime condition.
var ErrConnParked = errors.New("connection is parked for an approval hold")

// Park starts the read-watch. The returned channel is closed when the client
// goes away (EOF, reset, or any other terminal read error). Unpark must be
// called before the session resumes reading.
func (w *WatchedConn) Park() <-chan struct{} {
	gone := make(chan struct{})

	w.mu.Lock()

	if w.parked {
		w.mu.Unlock()
		close(gone)

		return gone
	}

	// Already known dead: report immediately rather than spinning a watcher.
	if w.err != nil {
		w.mu.Unlock()
		close(gone)

		return gone
	}

	w.parked = true
	stopped := make(chan struct{})
	w.stopped = stopped
	w.mu.Unlock()

	// The watcher's own `defer close(stopped)` is what releases Unpark, which
	// blocks on it — that is RunGuarded's precondition. WatchedConn has no
	// logger of its own (it wraps a net.Conn, nothing more), so the panic
	// reports through slog.Default().
	go RunGuarded(context.Background(), nil, GoroutineNameParkWatcher, func() {
		w.watch(gone, stopped)
	})

	return gone
}

// watch reads into the replay queue until the socket dies or Unpark interrupts
// it with a zero read deadline.
func (w *WatchedConn) watch(gone, stopped chan struct{}) {
	defer close(stopped)

	buf := make([]byte, 4096)

	for {
		n, err := w.Conn.Read(buf)

		if n > 0 {
			w.mu.Lock()
			w.pending = append(w.pending, buf[:n]...)
			w.mu.Unlock()
		}

		if err == nil {
			continue
		}

		// A deadline timeout is how Unpark interrupts us — not a disconnect.
		if isTimeout(err) {
			return
		}

		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.parked = false
		w.mu.Unlock()

		close(gone)

		return
	}
}

// Unpark stops the read-watch and hands the socket back to the session. Any
// bytes the watcher captured stay queued for replay.
func (w *WatchedConn) Unpark() {
	w.mu.Lock()
	stopped := w.stopped
	parked := w.parked
	w.mu.Unlock()

	if !parked || stopped == nil {
		return
	}

	// Interrupt the blocked Read. Only a deadline can do that portably.
	_ = w.SetReadDeadline(time.Now())
	<-stopped
	_ = w.SetReadDeadline(time.Time{})

	w.mu.Lock()
	w.parked = false
	w.stopped = nil
	w.mu.Unlock()
}

// Buffered reports how many replay bytes are queued. Test/telemetry helper.
func (w *WatchedConn) Buffered() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.pending)
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	var nerr net.Error
	if errors.As(err, &nerr) {
		return nerr.Timeout()
	}

	return false
}

// Compile-time assertion that a closed peer surfaces as io.EOF through Read.
var _ = io.EOF
