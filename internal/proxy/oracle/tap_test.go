//go:build integration

package oracle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
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
// with the same walks the tail learner uses, so a frame is read back exactly the
// way the code under measurement writes one — and a real server's frame is read
// the same way as dbbat's.
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

		if decoded, ok := decodeTappedOER(payload); ok {
			out = append(out, decoded)

			continue
		}

		// Neither layout recognized the frame. Report it anyway from the
		// leading fields, which decode under a much weaker assumption — a frame
		// nobody can read is itself a finding, and dropping it silently would
		// make it look like nothing was sent.
		if info := decodeOERAt(payload, 0); info != nil {
			out = append(out, tappedOER{errorCode: info.ErrorCode, message: info.ErrorMessage})
		}
	}
}

// decodeTappedOER reads one summary object in whichever of the two encodings it
// is in: the compressed one every thin driver parses, or the fixed-width
// little-endian one OCI/sqlplus parses.
//
// Trying both is not belt-and-braces, and neither is arbitrating between them by
// the frame's own message. A fixed-width summary is mostly *zeroes*, and a run
// of zeroes walks cleanly as a sequence of compressed integers — so the
// compressed walk does not fail on an OCI frame, it succeeds and reports
// ORA-00000 on call 0. Measured: dbbat's ORA-00028 to the bundled 23.26 client
// read back as ORA-00000 with the right message text attached, which would have
// been recorded as "dbbat wrote a frame nobody can name" when what it really
// wrote was the frame that client went on to report correctly.
//
// The message is the arbiter because it is the one field neither encoding can
// fake: an OER carrying "ORA-00028: …" carries error code 28, so a walk that
// disagrees with the text lost the layout, whatever else it validated.
//
// A *messageless* OER — a plain end-of-call status, error code 0 — has no such
// arbiter, and the zero-run problem is exactly the same there, so the fallback
// is not "prefer the compressed walk". It is the **fixed-width** candidate,
// because the two validations are not equally strong: oerFixedWidthTailFieldsAt
// walks to the end of the payload and requires it to terminate there (message
// CLR, or the end-of-response marker, and nothing after), while
// skipOERFixedFields validates a *prefix* and says nothing about the rest. A
// short compressed status OER cannot satisfy the fixed layouts at all — the
// 32-bit one alone needs 71 bytes with the trailing RetCode repeating the
// leading error number — so it still falls through to the compressed decode.
func decodeTappedOER(payload []byte) (tappedOER, bool) {
	compressed, compressedOK := tappedCompressedOER(payload)
	fixed, fixedOK := tappedFixedWidthOER(payload)

	if wanted, ok := oraCodeInMessage(payload); ok {
		switch {
		case compressedOK && compressed.errorCode == wanted:
			return compressed, true
		case fixedOK && fixed.errorCode == wanted:
			return fixed, true
		}

		return tappedOER{}, false
	}

	if fixedOK {
		return fixed, true
	}

	return compressed, compressedOK
}

// tappedCompressedOER decodes the thin-client encoding through the very walk
// dbbat's own tail learner uses.
func tappedCompressedOER(payload []byte) (tappedOER, bool) {
	pos, errCode, callNumber, ok := skipOERFixedFields(defaultOERShape(), payload, 0)
	if !ok {
		return tappedOER{}, false
	}

	return tappedOER{
		errorCode:     errCode,
		callNumber:    callNumber,
		callNumberOK:  true,
		messageParsed: true,
		message:       extractORAMessage(payload[pos:]),
	}, true
}

// tappedFixedWidthOER decodes a summary object in the OCI/sqlplus encoding.
//
// Validation is delegated to oerFixedWidthTailFieldsAt — the same check the
// shape learner uses, at whichever of the two fixed layouts holds: the trailing
// RetCode has to repeat the leading error number and the call status has to be
// non-zero, so a run of row bytes cannot masquerade as one. Reading the fields
// off the layout afterwards is then reading them the way encodeOERFixedWidth
// wrote them.
func tappedFixedWidthOER(payload []byte) (tappedOER, bool) {
	tail, ok := oerFixedWidthTailFieldsAt(payload, 0)
	if !ok {
		return tappedOER{}, false
	}

	layout := oerShape{fixedWidth: true, fixedWidth64: tail.fixedWidth64}.layoutFor()

	return tappedOER{
		errorCode:     int(binary.LittleEndian.Uint16(payload[layout.errNum:])),
		callNumber:    byte(binary.LittleEndian.Uint32(payload[layout.callNumber:])),
		callNumberOK:  true,
		messageParsed: true,
		message:       extractORAMessage(payload[layout.prefixLen:]),
	}, true
}

// oraCodeInMessage reads the error number out of the "ORA-NNNNN: …" text a
// summary object carries, which is what lets the two candidate decodes above be
// arbitrated rather than guessed between. Not ok for a frame with no message —
// a status OER, which carries no code to disagree about either.
func oraCodeInMessage(payload []byte) (int, bool) {
	msg := extractORAMessage(payload)
	if !strings.HasPrefix(msg, "ORA-") || len(msg) < len("ORA-00000") {
		return 0, false
	}

	code, err := strconv.Atoi(msg[len("ORA-"):len("ORA-00000")])
	if err != nil {
		return 0, false
	}

	return code, true
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
