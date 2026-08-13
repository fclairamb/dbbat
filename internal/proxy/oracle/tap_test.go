//go:build integration

package oracle

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recordingTap is a raw TCP relay that keeps every byte the *server* side sent,
// per connection, and notes how that direction ended.
//
// It is the cheapest instrument for the questions this package keeps asking
// about asynchronous refusals, and the only one that answers them without
// taking the client's word for it: a driver reporting "connection closed by
// peer" cannot distinguish a frame that was never written from one that was
// written and lost, and those have opposite fixes. Pointed at dbbat's proxy it
// says what dbbat wrote; pointed at a real Oracle it says what Oracle sends when
// it kills a session, which is the reference the proxy's behavior is judged
// against.
//
// It copies bytes and nothing else — no framing, no parsing — so it cannot
// change what either end sees.
type recordingTap struct {
	host string
	port int

	mu      sync.Mutex
	records []*tapRecord
}

// tapRecord is one client connection's worth of tapped server→client traffic.
type tapRecord struct {
	mu sync.Mutex

	fromServer bytes.Buffer

	// serverClosed is set when the server end of this connection reached EOF or
	// was reset — i.e. the server hung up rather than answering.
	serverClosed bool
}

func (r *tapRecord) bytesFromServer() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]byte(nil), r.fromServer.Bytes()...)
}

// bytesSeen is how many bytes the server side has sent so far. Sampled either
// side of a pause, it answers "did the server push anything while the client
// was idle?" — which is the whole question on the watchdog path.
func (r *tapRecord) bytesSeen() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.fromServer.Len()
}

func (r *tapRecord) closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.serverClosed
}

func (r *tapRecord) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.fromServer.Write(p)
}

// startRecordingTap listens on loopback and relays every accepted connection to
// target, recording the server→client direction. The listener is closed with the
// test.
func startRecordingTap(t *testing.T, targetHost string, targetPort int) *recordingTap {
	t.Helper()

	return startRecordingTapOn(t, "127.0.0.1", targetHost, targetPort)
}

// startRecordingTapOn is startRecordingTap with the bind address spelled out,
// for the one client that cannot reach loopback: sqlplus bundled in the Oracle
// container dials back through host.docker.internal, so a tap it is to go
// through has to be bound on every interface the way the proxy itself is under
// oracleFixtureOptions.reachableFromContainers.
func startRecordingTapOn(t *testing.T, bindHost, targetHost string, targetPort int) *recordingTap {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	host, portStr, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	// A wildcard bind reports 0.0.0.0, which is not an address to hand a client.
	// Every in-process client still goes to loopback; the one that cannot
	// (sqlplus inside the Oracle container) dials this port on
	// host.docker.internal rather than on tap.host.
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}

	tap := &recordingTap{host: host, port: port}
	target := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))

	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go tap.relay(client, target)
		}
	}()

	return tap
}

func (tap *recordingTap) relay(client net.Conn, target string) {
	defer func() { _ = client.Close() }()

	upstream, err := net.Dial("tcp", target)
	if err != nil {
		return
	}

	defer func() { _ = upstream.Close() }()

	record := &tapRecord{}

	tap.mu.Lock()
	tap.records = append(tap.records, record)
	tap.mu.Unlock()

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		_, _ = io.Copy(upstream, client)

		// Half-close so the server sees the client's FIN rather than a reset,
		// which is what an ordinary proxy does and what keeps a polite server
		// shutdown observable as one.
		if cw, ok := upstream.(*net.TCPConn); ok {
			_ = cw.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()

		_, copyErr := io.Copy(io.MultiWriter(client, record), upstream)

		record.mu.Lock()
		record.serverClosed = copyErr == nil || errors.Is(copyErr, io.EOF) ||
			errors.Is(copyErr, net.ErrClosed) || isConnResetError(copyErr)
		record.mu.Unlock()

		if cw, ok := client.(*net.TCPConn); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()
}

func isConnResetError(err error) bool {
	var opErr *net.OpError

	return errors.As(err, &opErr)
}

// lastRecord returns the most recently accepted connection's record. Probes open
// exactly one connection, so "last" is "the probe's".
func (tap *recordingTap) lastRecord(t *testing.T) *tapRecord {
	t.Helper()

	tap.mu.Lock()
	defer tap.mu.Unlock()

	require.NotEmpty(t, tap.records, "nothing ever connected through the tap")

	return tap.records[len(tap.records)-1]
}

// tappedOERs decodes every TTC OER the server side sent, in order, from a tapped
// byte stream: the error code it carries and the call number it stamps.
//
// TNS framing is parsed with this package's own reader, and the summary object
// with the walk the tail learner uses (skipOERFixedFields), so a frame is read
// back exactly the way the code under measurement writes one — and a real
// server's frame is read the same way as dbbat's.
func tappedOERs(t *testing.T, stream []byte) []tappedOER {
	t.Helper()

	var out []tappedOER

	reader := &replayConn{Reader: bytes.NewReader(stream)}

	for {
		pkt, err := readTNSPacket(reader)
		if err != nil {
			return out
		}

		if pkt.Type != TNSPacketTypeData || len(pkt.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		payload := extractTTCPayload(pkt.Payload)
		if len(payload) == 0 || payload[0] != byte(TTCFuncOERR) {
			continue
		}

		pos, errCode, callNumber, ok := skipOERFixedFields(defaultOERShape(), payload, 0)
		if !ok {
			// The compressed walk did not recognize the layout. Report the OER
			// anyway from the leading fields, which decode under a much weaker
			// assumption — a frame nobody can read is itself a finding, and
			// dropping it silently would make it look like nothing was sent.
			if info := decodeOERAt(payload, 0); info != nil {
				out = append(out, tappedOER{errorCode: info.ErrorCode, message: info.ErrorMessage})
			}

			continue
		}

		out = append(out, tappedOER{
			errorCode:     errCode,
			callNumber:    callNumber,
			callNumberOK:  true,
			messageParsed: true,
			message:       extractORAMessage(payload[pos:]),
		})
	}
}

// tappedPacketTypes lists the TNS packet types in a tapped stream, in order. It
// is what shows a marker (break) exchange for what it is rather than as
// unexplained bytes ahead of an error frame.
func tappedPacketTypes(stream []byte) []TNSPacketType {
	var out []TNSPacketType

	reader := &replayConn{Reader: bytes.NewReader(stream)}

	for {
		pkt, err := readTNSPacket(reader)
		if err != nil {
			return out
		}

		out = append(out, pkt.Type)
	}
}

// replayConn makes a recorded byte stream readable by readTNSPacket, which
// takes a net.Conn but only ever reads from it. Reusing the production reader
// is the point: a tapped frame is then parsed by the same code that parses a
// live one, so a framing bug cannot hide behind a second implementation.
type replayConn struct {
	io.Reader
}

func (c *replayConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (c *replayConn) Close() error                     { return nil }
func (c *replayConn) LocalAddr() net.Addr              { return nil }
func (c *replayConn) RemoteAddr() net.Addr             { return nil }
func (c *replayConn) SetDeadline(time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(time.Time) error { return nil }

type tappedOER struct {
	errorCode  int
	callNumber byte
	message    string

	// callNumberOK is false when the summary object did not walk cleanly, in
	// which case callNumber is meaningless rather than zero-and-meaningful.
	callNumberOK  bool
	messageParsed bool
}
