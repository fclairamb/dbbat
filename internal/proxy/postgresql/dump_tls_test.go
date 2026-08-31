package postgresql

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// wireRecorder is the raw socket, underneath everything: it records every byte
// that actually crosses the network. negotiateSSL wraps it in the *tls.Conn, so
// what lands here after the upgrade is TLS records — which is exactly what a
// capture must NOT contain.
type wireRecorder struct {
	net.Conn

	mu    sync.Mutex
	bytes bytes.Buffer
}

func (w *wireRecorder) record(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.bytes.Write(b)
}

func (w *wireRecorder) Read(b []byte) (int, error) {
	n, err := w.Conn.Read(b)
	if n > 0 {
		w.record(b[:n])
	}

	return n, err
}

func (w *wireRecorder) Write(b []byte) (int, error) {
	n, err := w.Conn.Write(b)
	if n > 0 {
		w.record(b[:n])
	}

	return n, err
}

// seen returns everything that crossed the raw socket, both directions.
func (w *wireRecorder) seen() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()

	return bytes.Clone(w.bytes.Bytes())
}

// TestAttachDumpTaps_RecordsPlaintextAboveTLS pins the claim attachDumpTaps and
// docs/dump-format.md ("Captures are plaintext, above TLS") both make: on a
// TLS-upgraded PostgreSQL session the capture holds plaintext protocol
// messages, never TLS records.
//
// The ordering is the whole argument — negotiateSSL runs at the top of Run and
// replaces s.clientConn/s.clientReader with the TLS-wrapped pair long before
// the capture is opened — so this test runs the real negotiation with a real
// TLS client rather than a fake conn: both taps are then installed over
// whatever negotiateSSL actually left behind.
//
// The discriminating assertions are the two markers: each appears verbatim in
// the capture and neither appears on the raw socket. Taps installed *below* TLS
// would invert exactly that.
//
//nolint:maintidx // One scripted TLS session; splitting it would hide the ordering under test.
func TestAttachDumpTaps_RecordsPlaintextAboveTLS(t *testing.T) {
	t.Parallel()

	const (
		clientMarker = "SELECT 'dbbat-plaintext-above-tls-client-marker'"
		serverMarker = "dbbat-plaintext-above-tls-server-marker"
	)

	tlsConf, err := generateSelfSignedTLS()
	require.NoError(t, err)

	clientSide, serverSide := net.Pipe()

	defer func() { _ = clientSide.Close() }()
	defer func() { _ = serverSide.Close() }()

	wire := &wireRecorder{Conn: serverSide}
	session := minimalSession(wire, tlsConf)

	query, err := (&pgproto3.Query{String: clientMarker}).Encode(nil)
	require.NoError(t, err)

	// The client: SSLRequest, TLS handshake, one Query, then read whatever the
	// server answers. net.Pipe is unbuffered, so the Query below cannot cross
	// before the server reads it — which happens only after attachDumpTaps has
	// run, exactly as in a live session.
	clientErrCh := make(chan error, 1)
	clientGotCh := make(chan []byte, 1)

	go func() {
		clientErrCh <- func() error {
			if _, err := clientSide.Write(makeSSLRequest()); err != nil {
				return err
			}

			resp := make([]byte, 1)
			if _, err := io.ReadFull(clientSide, resp); err != nil {
				return err
			}

			if resp[0] != 'S' {
				return io.ErrUnexpectedEOF
			}

			clientTLS := tls.Client(clientSide, &tls.Config{
				InsecureSkipVerify: true, // self-signed test cert
				MinVersion:         tls.VersionTLS12,
			})
			if err := clientTLS.Handshake(); err != nil {
				return err
			}

			if _, err := clientTLS.Write(query); err != nil {
				return err
			}

			got := make([]byte, 512)

			n, err := clientTLS.Read(got)
			if err != nil {
				return err
			}

			clientGotCh <- got[:n]

			return nil
		}()
	}()

	require.NoError(t, session.negotiateSSL())
	require.IsType(t, &tls.Conn{}, session.clientConn,
		"negotiateSSL should have replaced clientConn with the *tls.Conn")

	dir := t.TempDir()
	uid := uuid.New()
	path := filepath.Join(dir, uid.String()+dump.FileExt)

	dw, err := dump.NewWriter(path, dump.Header{
		SessionID:  uid.String(),
		Protocol:   dump.ProtocolPostgreSQL,
		StartTime:  time.Now(),
		Connection: map[string]any{"database": "app"},
	}, 10<<20)
	require.NoError(t, err)

	// The call under test — the same one Run makes, at the same point: after
	// negotiateSSL, so what it wraps is already the TLS pair.
	session.attachDumpTaps(dw)

	msg, err := session.clientBackend.Receive()
	require.NoError(t, err, "server should read the client's Query through the tapped reader")
	require.IsType(t, &pgproto3.Query{}, msg)

	errResp, err := (&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "42501",
		Message:  serverMarker,
	}).Encode(nil)
	require.NoError(t, err)

	_, err = session.clientConn.Write(errResp)
	require.NoError(t, err, "server should answer through the tapped conn")

	select {
	case err := <-clientErrCh:
		require.NoError(t, err, "client side")
	case <-time.After(5 * time.Second):
		t.Fatal("client script hung")
	}

	require.NoError(t, dw.Close())

	_, err = os.Stat(path)
	require.NoError(t, err, "capture file should exist at %s", path)

	pkts := readCapture(t, path)
	require.NotEmpty(t, pkts, "capture should hold the message exchange")

	toServer := captureStream(pkts, dump.DirClientToServer)
	toClient := captureStream(pkts, dump.DirServerToClient)

	// Plaintext, above TLS: both directions are legible in the capture...
	assert.Equal(t, byte('Q'), toServer[0], "first captured client→server byte should be the Query tag")
	assert.Contains(t, string(toServer), clientMarker,
		"the capture should hold the plaintext Query, not a TLS record")
	assert.Equal(t, byte('E'), toClient[0], "first captured server→client byte should be the ErrorResponse tag")
	assert.Contains(t, string(toClient), serverMarker,
		"the capture should hold the plaintext ErrorResponse, not a TLS record")

	// ...and unreadable on the wire underneath, which is what proves the
	// session really was encrypted and the taps really sat above the crypto.
	seen := string(wire.seen())
	assert.NotContains(t, seen, clientMarker,
		"the Query must not be legible on the raw socket — otherwise TLS never engaged")
	assert.NotContains(t, seen, serverMarker,
		"the ErrorResponse must not be legible on the raw socket")
	assert.Contains(t, seen, string([]byte{0x17, 0x03, 0x03}),
		"the raw socket should carry TLS application-data records")

	// Sanity: what the client decrypted is what the capture recorded.
	select {
	case got := <-clientGotCh:
		assert.Equal(t, errResp, got, "client should have decrypted the same bytes the tap recorded")
	default:
		t.Error("client never reported the server's answer")
	}
}

// readCapture reads every packet out of a finished capture file.
func readCapture(t *testing.T, path string) []dump.Packet {
	t.Helper()

	r, err := dump.OpenReader(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	var out []dump.Packet

	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)
		}

		out = append(out, *pkt)
	}

	return out
}

// captureStream concatenates one direction of a capture back into the byte
// stream the tap was handed.
func captureStream(pkts []dump.Packet, dir byte) []byte {
	var out []byte

	for _, pkt := range pkts {
		if pkt.Direction == dir {
			out = append(out, pkt.Data...)
		}
	}

	return out
}
