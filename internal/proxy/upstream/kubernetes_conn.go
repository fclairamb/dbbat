package upstream

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// requestIDs numbers the stream pairs multiplexed over one port-forward
// connection. The API server uses the requestID header to pair a data stream
// with its error stream, so the two streams of one connection must share an id
// and no two live pairs may collide.
var requestIDs atomic.Int64

// portForwardConn adapts one `pods/portforward` stream pair — a data stream
// plus the error stream the kubelet reports forwarding failures on — to
// net.Conn, so every proxy can treat a pod exactly like a dialed TCP endpoint.
//
// One database session owns one stream pair *and* the upgraded connection
// carrying it: unlike SSH, where many channels ride one pooled *ssh.Client, a
// port-forward upgrade is per target pod, so there is nothing to share between
// sessions that may be pointed at different pods. What the tunnel pools is the
// client and its TLS material, not the stream connection.
type portForwardConn struct {
	stream    httpstream.Stream
	errStream httpstream.Stream
	conn      httpstream.Connection

	local  podAddr
	remote podAddr

	// forwardErr holds whatever the kubelet wrote to the error stream. That
	// stream is where "connection refused" for the *container* port shows up:
	// the upgrade itself succeeds, and without surfacing it a wrong port would
	// look to the proxy like an upstream that accepted then immediately hung up.
	forwardErrMu sync.Mutex
	forwardErr   error
	errDone      chan struct{}

	closeOnce sync.Once
	closeErr  error
}

var _ net.Conn = (*portForwardConn)(nil)

// newPortForwardConn creates the error and data streams for one forwarded
// connection and wraps the data stream as a net.Conn.
func newPortForwardConn(conn httpstream.Connection, namespace, podName string, port int) (*portForwardConn, error) {
	requestID := requestIDs.Add(1) - 1

	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(port))
	headers.Set(corev1.PortForwardRequestIDHeader, strconv.FormatInt(requestID, 10))

	// The error stream must be created first: the API server pairs the data
	// stream to it by requestID, and creating them the other way round races.
	errStream, err := conn.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: failed to create the port-forward error stream: %w", err)
	}
	// We never write to the error stream; half-closing it tells the kubelet so.
	_ = errStream.Close()

	headers.Set(corev1.StreamType, corev1.StreamTypeData)

	dataStream, err := conn.CreateStream(headers)
	if err != nil {
		conn.RemoveStreams(errStream)

		return nil, fmt.Errorf("kubernetes: failed to create the port-forward data stream: %w", err)
	}

	addr := podAddr{namespace: namespace, pod: podName, port: port}

	pfc := &portForwardConn{
		stream:    dataStream,
		errStream: errStream,
		conn:      conn,
		local:     podAddr{namespace: namespace, pod: "dbbat", port: 0},
		remote:    addr,
		errDone:   make(chan struct{}),
	}

	go pfc.watchErrorStream()

	return pfc, nil
}

// watchErrorStream drains the error stream for the life of the connection. A
// non-empty payload is the kubelet refusing to forward (no such container port,
// pod gone); it is recorded and reported in place of the bare EOF the data
// stream would otherwise give.
func (c *portForwardConn) watchErrorStream() {
	defer close(c.errDone)

	message, err := io.ReadAll(c.errStream)

	switch {
	case len(message) > 0:
		c.setForwardErr(fmt.Errorf("kubernetes: port-forward to %s failed: %s", c.remote, strings.TrimSpace(string(message))))
	case err != nil && !errors.Is(err, io.EOF):
		c.setForwardErr(fmt.Errorf("kubernetes: port-forward error stream for %s: %w", c.remote, err))
	}
}

func (c *portForwardConn) setForwardErr(err error) {
	c.forwardErrMu.Lock()
	if c.forwardErr == nil {
		c.forwardErr = err
	}
	c.forwardErrMu.Unlock()
}

func (c *portForwardConn) recordedForwardErr() error {
	c.forwardErrMu.Lock()
	defer c.forwardErrMu.Unlock()

	return c.forwardErr
}

// Read reads forwarded bytes. On EOF it gives the error stream a brief moment
// to explain itself, so a rejected forward reports the kubelet's reason instead
// of an unexplained clean close.
func (c *portForwardConn) Read(b []byte) (int, error) {
	n, err := c.stream.Read(b)
	if err == nil || n > 0 {
		return n, err
	}

	if forwardErr := c.awaitForwardErr(); forwardErr != nil {
		return n, forwardErr
	}

	return n, err
}

// Write writes bytes to be forwarded to the container port.
func (c *portForwardConn) Write(b []byte) (int, error) {
	n, err := c.stream.Write(b)
	if err != nil {
		if forwardErr := c.recordedForwardErr(); forwardErr != nil {
			return n, forwardErr
		}
	}

	return n, err
}

// awaitForwardErr returns the kubelet's forwarding error, waiting a moment for
// the error stream to close if it has not already. The wait is bounded because
// a healthy connection's error stream stays open for the whole session: only an
// already-finished read is informative here.
func (c *portForwardConn) awaitForwardErr() error {
	select {
	case <-c.errDone:
	case <-time.After(200 * time.Millisecond):
	}

	return c.recordedForwardErr()
}

// Close tears down the stream pair and the upgraded connection under it. The
// connection belongs to this conn alone (see the type comment), so leaving it
// open would leak one HTTPS connection to the API server per database session.
func (c *portForwardConn) Close() error {
	c.closeOnce.Do(func() {
		// Reset before Close: unsent data on a half-written stream can
		// otherwise wedge the teardown.
		_ = c.stream.Reset()
		c.closeErr = c.stream.Close()
		c.conn.RemoveStreams(c.stream, c.errStream)
		_ = c.conn.Close()
	})

	return c.closeErr
}

// LocalAddr reports a synthetic address naming this end of the tunnel.
func (c *portForwardConn) LocalAddr() net.Addr { return c.local }

// RemoteAddr reports the forwarded pod and container port.
func (c *portForwardConn) RemoteAddr() net.Addr { return c.remote }

// SetDeadline and friends are not supported: a SPDY stream carries no timer of
// its own. This matches the SSH channel conns the bastion path already returns
// (golang.org/x/crypto/ssh rejects SetDeadline the same way), so every caller
// that already copes with a bastion conn copes with this one.
func (c *portForwardConn) SetDeadline(time.Time) error { return os.ErrNoDeadline }

// SetReadDeadline is not supported; see SetDeadline.
func (c *portForwardConn) SetReadDeadline(time.Time) error { return os.ErrNoDeadline }

// SetWriteDeadline is not supported; see SetDeadline.
func (c *portForwardConn) SetWriteDeadline(time.Time) error { return os.ErrNoDeadline }

// podAddr is a net.Addr naming a pod's container port. Port-forwarding has no
// IP address to report — the stream terminates inside the kubelet — so the
// address is the identity that actually means something to an operator.
type podAddr struct {
	namespace string
	pod       string
	port      int
}

var _ net.Addr = podAddr{}

// Network names the transport, mirroring how "tcp" identifies a dialed socket.
func (a podAddr) Network() string { return "kubernetes-portforward" }

func (a podAddr) String() string {
	if a.port == 0 {
		return a.namespace + "/" + a.pod
	}

	return a.namespace + "/" + a.pod + ":" + strconv.Itoa(a.port)
}
