package mysql

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// wireRecorder is the raw socket, underneath everything: it records every byte
// that actually crosses the network. go-mysql wraps it in the *tls.Conn during
// the handshake, so what lands here after the upgrade is TLS records — which is
// exactly what a capture must NOT contain.
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

// tlsServerConnResult carries a TLS-upgraded server-side handshake back to the
// test goroutine, along with the raw socket recorder underneath it.
type tlsServerConnResult struct {
	conn *gomysqlserver.Conn
	wire *wireRecorder
	err  error
}

// acceptTLSServerConn accepts one connection and drives the go-mysql
// server-side handshake over it, with TLS available. The credential is
// registered with mysql_native_password to match NewDefaultServer's default
// auth method, keeping the handshake on its single-round-trip path (see
// acceptServerConn in upstream_wiring_test.go for why that matters).
func acceptTLSServerConn(ln net.Listener, user, password string) <-chan tlsServerConnResult {
	resultCh := make(chan tlsServerConnResult, 1)

	go func() {
		netConn, err := ln.Accept()
		if err != nil {
			resultCh <- tlsServerConnResult{err: err}

			return
		}

		wire := &wireRecorder{Conn: netConn}

		// NewDefaultServer advertises CLIENT_SSL with an auto-generated
		// certificate, which is what makes the client's SSLRequest branch
		// reachable — the same branch dbbat's own server config enables.
		srv := gomysqlserver.NewDefaultServer()

		authHandler := gomysqlserver.NewInMemoryAuthenticationHandler(gomysql.AUTH_NATIVE_PASSWORD)
		if err := authHandler.AddUser(user, password, gomysql.AUTH_NATIVE_PASSWORD); err != nil {
			resultCh <- tlsServerConnResult{err: err}

			return
		}

		conn, err := srv.NewCustomizedConn(wire, authHandler, stubCommandHandler{})
		resultCh <- tlsServerConnResult{conn: conn, wire: wire, err: err}
	}()

	return resultCh
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

// TestStartDumpIfConfigured_RecordsPlaintextAboveTLS pins the claim
// startDumpIfConfigured and docs/dump-format.md ("Captures are plaintext, above
// TLS") both make: on a TLS-upgraded MySQL session the capture holds plaintext
// MySQL packets, never TLS records.
//
// The claim rests on go-mysql's internals — server/handshake_resp.go swaps
// c.Conn.Conn for the *tls.Conn, and startDumpIfConfigured wraps that same
// field — so this test uses a real TLS handshake against a real
// gomysqlserver.Conn rather than a fake: a go-mysql upgrade that installed the
// *tls.Conn anywhere else would flip the assertions here instead of silently
// turning every encrypted session's capture into opaque TLS records.
//
// The discriminating assertion is the marker query text: it appears verbatim in
// the capture and nowhere on the raw socket. A tap installed *below* TLS would
// invert exactly that.
func TestStartDumpIfConfigured_RecordsPlaintextAboveTLS(t *testing.T) {
	t.Parallel()

	const (
		username = "florent"
		password = "clientpass"
		marker   = "SELECT 'dbbat-plaintext-above-tls-marker'"
	)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { _ = ln.Close() }()

	serverConnCh := acceptTLSServerConn(ln, username, password)

	client, err := gomysqlclient.Connect(ln.Addr().String(), username, password, "",
		func(c *gomysqlclient.Conn) error {
			c.UseSSL(true)

			return nil
		})
	require.NoError(t, err, "TLS client handshake through go-mysql")

	defer func() { _ = client.Close() }()

	res := <-serverConnCh
	require.NoError(t, res.err, "server-side handshake")

	// The invariant everything below depends on: after the upgrade go-mysql
	// has installed the *tls.Conn at exactly the field dbbat wraps. If a
	// go-mysql release moves it, this is where that shows up.
	require.IsType(t, &tls.Conn{}, res.conn.Conn.Conn,
		"go-mysql should have swapped c.Conn.Conn for the *tls.Conn (server/handshake_resp.go)")

	dir := t.TempDir()
	uid := uuid.New()

	session := &Session{
		server: &Server{
			dumpConfig: config.DumpConfig{Dir: dir, MaxSize: config.DefaultDumpMaxSize},
		},
		serverConn: res.conn,
		connection: &store.Connection{UID: uid},
		user:       &store.User{Username: username},
		database: &store.Server{
			DatabaseName: "app",
			Host:         "127.0.0.1",
			Port:         3306,
			Protocol:     store.ProtocolMySQL,
		},
		logger: slog.New(slog.DiscardHandler),
		ctx:    context.Background(),
	}

	// The call under test — the same one Run makes, at the same point: after
	// the handshake, so the conn it wraps is already the *tls.Conn.
	session.startDumpIfConfigured()
	require.NotNil(t, session.dumpWriter, "dump writer should have been opened")

	// One real command round trip through the tapped conn. The stub handler
	// refuses the query, so the server answers with an ERR packet: one
	// plaintext MySQL packet in each direction.
	queryErrCh := make(chan error, 1)

	go func() {
		_, execErr := client.Execute(marker)
		queryErrCh <- execErr
	}()

	require.NoError(t, res.conn.HandleCommand(), "server should answer the command")

	select {
	case execErr := <-queryErrCh:
		require.Error(t, execErr, "the stub handler refuses every query")
	case <-time.After(5 * time.Second):
		t.Fatal("client Execute hung")
	}

	require.NoError(t, session.dumpWriter.Close())

	path := filepath.Join(dir, uid.String()+dump.FileExt)
	_, err = os.Stat(path)
	require.NoError(t, err, "capture file should exist at %s", path)

	pkts := readCapture(t, path)
	require.NotEmpty(t, pkts, "capture should hold the command round trip")

	toServer := captureStream(pkts, dump.DirClientToServer)
	toClient := captureStream(pkts, dump.DirServerToClient)

	// Plaintext, above TLS: the query text is legible in the capture...
	assert.Contains(t, string(toServer), marker,
		"the capture should hold the plaintext COM_QUERY, not a TLS record")
	assert.Equal(t, gomysql.COM_QUERY, mysqlPacketBody(t, toServer)[0],
		"first captured client→server packet should be a COM_QUERY")
	assert.Equal(t, gomysql.ERR_HEADER, mysqlPacketBody(t, toClient)[0],
		"first captured server→client packet should be the ERR packet")

	// ...and unreadable on the wire underneath it, which is what proves the
	// session really was encrypted and the tap really sat above the crypto.
	wire := res.wire.seen()
	assert.NotContains(t, string(wire), marker,
		"the query must not be legible on the raw socket — otherwise TLS never engaged")
	assert.Contains(t, string(wire), string([]byte{0x17, 0x03, 0x03}),
		"the raw socket should carry TLS application-data records")
}

// mysqlPacketBody validates that stream starts with a MySQL packet
// (3-byte little-endian length, sequence byte, body) and returns its body. It
// fails the test on anything that is not framed that way — a TLS record
// included, which is the point.
func mysqlPacketBody(t *testing.T, stream []byte) []byte {
	t.Helper()

	const headerLen = 4

	require.GreaterOrEqual(t, len(stream), headerLen, "stream too short to hold a MySQL packet header")

	length := int(stream[0]) | int(stream[1])<<8 | int(stream[2])<<16
	require.Positive(t, length, "MySQL packet body should not be empty")
	require.LessOrEqual(t, headerLen+length, len(stream),
		"declared MySQL packet length %d overruns the captured stream", length)

	return stream[headerLen : headerLen+length]
}
