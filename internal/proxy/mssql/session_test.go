package mssql

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// testLogger discards output; the tests assert on the wire, not on logs.
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// newTestServer builds a proxy with no store, auth cache or capture: the
// handshake suite exercises the wire, not the pipeline behind it.
func newTestServer(t *testing.T, cfg config.MSSQLConfig) *Server {
	t.Helper()

	srv, err := NewServer(nil, nil, config.DumpConfig{}, nil, cfg, testLogger())
	require.NoError(t, err)

	return srv
}

// startTestServer brings up the proxy on an ephemeral port and returns its
// address.
//
// It is built without a store, so every login that gets past the handshake is
// refused as an authentication failure. That is exactly what these tests want
// to assert on: reaching the refusal proves the whole handshake worked. The
// store-backed path has its own suite in auth_test.go.
func startTestServer(t *testing.T, cfg config.MSSQLConfig) string {
	t.Helper()

	srv := newTestServer(t, cfg)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv.setListener(listener)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go srv.handleConnection(conn)
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
	})

	return listener.Addr().String()
}

// tdsErrorToken is the decoded ERROR token a client would surface.
type tdsErrorToken struct {
	Number  int32
	State   byte
	Class   byte
	Message string
	Server  string
}

// parseLoginFailure decodes the ERROR + DONE stream the proxy sends when it
// refuses a login. It is a miniature client-side token reader: proving the
// bytes parse this way is proving a real driver can read them.
func parseLoginFailure(t *testing.T, payload []byte) tdsErrorToken {
	t.Helper()

	require.NotEmpty(t, payload)
	require.Equal(t, tokenError, payload[0], "the stream must start with an ERROR token")

	length := int(binary.LittleEndian.Uint16(payload[1:3]))
	require.GreaterOrEqual(t, len(payload), 3+length)

	body := payload[3 : 3+length]

	tok := tdsErrorToken{
		Number: int32(binary.LittleEndian.Uint32(body[0:4])),
		State:  body[4],
		Class:  body[5],
	}

	pos := 6

	msgChars := int(binary.LittleEndian.Uint16(body[pos : pos+2]))
	pos += 2
	tok.Message = ucs2ToString(body[pos : pos+msgChars*2])
	pos += msgChars * 2

	srvChars := int(body[pos])
	pos++
	tok.Server = ucs2ToString(body[pos : pos+srvChars*2])
	pos += srvChars * 2

	procChars := int(body[pos])
	pos++
	pos += procChars * 2

	// LineNumber is 4 bytes on TDS 7.2+.
	require.Len(t, body, pos+4, "the ERROR token body must be fully consumed")

	// A DONE token flagged as an error must follow, or clients wait forever.
	done := payload[3+length:]
	require.Len(t, done, 13)
	require.Equal(t, tokenDone, done[0])
	assert.Equal(t, doneError, binary.LittleEndian.Uint16(done[1:3]))

	return tok
}

// clientPreloginPayload builds a client PRELOGIN offering the given encryption.
func clientPreloginPayload(encryption byte, mars bool) []byte {
	marsByte := byte(0x00)
	if mars {
		marsByte = 0x01
	}

	return synthPrelogin([]preloginOption{
		{Token: preloginVersion, Data: []byte{0x10, 0x00, 0x0F, 0xA3, 0x00, 0x00}},
		{Token: preloginEncryption, Data: []byte{encryption}},
		{Token: preloginInstOpt, Data: append([]byte("MSSQLServer"), 0x00)},
		{Token: preloginThreadID, Data: []byte{0x01, 0x00, 0x00, 0x00}},
		{Token: preloginMARS, Data: []byte{marsByte}},
	})
}

// testClient is a minimal TDS client: enough to exercise the full handshake,
// including the encapsulated TLS one.
type testClient struct {
	conn   net.Conn
	stream *revertibleConn
	pkt    *packetRW
}

func dialTestClient(t *testing.T, addr string) *testClient {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Now().Add(15*time.Second)))

	t.Cleanup(func() {
		_ = conn.Close()
	})

	stream := newRevertibleConn(conn)

	return &testClient{conn: conn, stream: stream, pkt: newPacketRW(stream)}
}

// prelogin performs the PRELOGIN exchange and returns the server's response.
func (c *testClient) prelogin(t *testing.T, encryption byte, mars bool) *preloginMessage {
	t.Helper()

	require.NoError(t, c.pkt.WriteMessage(packetTypePrelogin, clientPreloginPayload(encryption, mars)))

	msgType, payload, err := c.pkt.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, packetTypeReply, msgType,
		"the PRELOGIN response is a Tabular Result packet, not a PRELOGIN one")

	resp, err := parsePrelogin(payload)
	require.NoError(t, err)

	return resp
}

// tlsHandshake performs the encapsulated TLS handshake from the client side and
// puts the TLS session in force.
func (c *testClient) tlsHandshake(t *testing.T) *tls.Conn {
	t.Helper()

	adapter := newHandshakeConn(c.conn, c.pkt)
	tlsConn := tls.Client(adapter, &tls.Config{
		InsecureSkipVerify: true, // the proxy's auto-generated self-signed cert
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tlsMaxVersion,
	})

	require.NoError(t, tlsConn.Handshake())
	require.NoError(t, adapter.deactivate())

	c.stream.switchTo(tlsConn)

	return tlsConn
}

// sendLogin7 writes a LOGIN7 message.
func (c *testClient) sendLogin7(t *testing.T, login *Login7) {
	t.Helper()

	require.NoError(t, c.pkt.WriteMessage(packetTypeLogin7, login.serialize()))
}

// readReply reads one tabular-result message.
func (c *testClient) readReply(t *testing.T) []byte {
	t.Helper()

	msgType, payload, err := c.pkt.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, packetTypeReply, msgType)

	return payload
}

func TestSessionHandshakeWithFullEncryption(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)

	resp := client.prelogin(t, encryptOn, false)
	assert.Equal(t, encryptOn, resp.Encryption())

	tlsConn := client.tlsHandshake(t)
	assert.Equal(t, uint16(tls.VersionTLS12), tlsConn.ConnectionState().Version)

	client.sendLogin7(t, sampleLogin7())

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
	assert.Equal(t, "dbbat", tok.Server)
	assert.Contains(t, tok.Message, "Login failed for user 'florent'")
}

// TestSessionHandshakeWithLoginOnlyEncryption covers the row of the matrix that
// catches people out: Encrypt=no still runs a TLS handshake, LOGIN7 travels
// inside it, and the reply comes back in cleartext.
func TestSessionHandshakeWithLoginOnlyEncryption(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)

	resp := client.prelogin(t, encryptOff, false)
	assert.Equal(t, encryptOff, resp.Encryption())

	client.tlsHandshake(t)
	client.sendLogin7(t, sampleLogin7())

	// Both ends drop back to cleartext once the login packet is through.
	client.stream.revert()

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
}

func TestSessionHandshakeWithoutEncryption(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)

	resp := client.prelogin(t, encryptNotSup, false)
	assert.Equal(t, encryptNotSup, resp.Encryption())

	client.sendLogin7(t, sampleLogin7())

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
}

func TestSessionWithTLSDisabledAnswersNotSupported(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}})

	client := dialTestClient(t, addr)

	resp := client.prelogin(t, encryptOff, false)
	assert.Equal(t, encryptNotSup, resp.Encryption(),
		"a listener without TLS says so, and the client stays in cleartext")

	client.sendLogin7(t, sampleLogin7())

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
}

func TestSessionRefusesEncryptionRequiredWhenTLSDisabled(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}})

	client := dialTestClient(t, addr)

	// The client still gets an answer explaining the refusal, rather than a
	// silently dropped socket.
	resp := client.prelogin(t, encryptOn, false)
	assert.Equal(t, encryptNotSup, resp.Encryption())

	// The session then ends; the next read fails.
	_, _, err := client.pkt.ReadMessage()
	require.Error(t, err)
}

func TestSessionRefusesMARS(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)

	resp := client.prelogin(t, encryptNotSup, true)

	mars, ok := resp.get(preloginMARS)
	require.True(t, ok)
	assert.Equal(t, []byte{0x00}, mars, "MARS is refused in PRELOGIN")
}

func TestSessionRejectsIntegratedSecurityLogin(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)
	client.prelogin(t, encryptNotSup, false)

	login := sampleLogin7()
	login.OptionFlags2 |= optionFlags2IntSecurity
	client.sendLogin7(t, login)

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
	assert.Contains(t, tok.Message, "integrated")
	assert.Contains(t, tok.Message, "SQL login")
}

func TestSessionRejectsATDSVersionBelowTheFloor(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)
	client.prelogin(t, encryptNotSup, false)

	login := sampleLogin7()
	login.TDSVersion = tdsVersion72
	client.sendLogin7(t, login)

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
	assert.Contains(t, tok.Message, "7.4")
}

func TestSessionRejectsAPreTDS7Login(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)

	require.NoError(t, client.pkt.WriteMessage(packetTypeLegacyAuth, []byte("legacy")))

	_, _, err := client.pkt.ReadMessage()
	require.Error(t, err, "a non-PRELOGIN first message ends the session")
}

func TestSessionSurvivesAnImmediateDisconnect(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	// The listener must still serve the next client.
	client := dialTestClient(t, addr)
	client.prelogin(t, encryptNotSup, false)
	client.sendLogin7(t, sampleLogin7())

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
}

func TestSessionHonoursTheNegotiatedPacketSize(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, config.MSSQLConfig{})

	client := dialTestClient(t, addr)
	client.prelogin(t, encryptNotSup, false)

	login := sampleLogin7()
	login.PacketSize = minPacketSize
	client.sendLogin7(t, login)

	// The reply is small enough to fit either way; what matters is that the
	// client, reading at the size it asked for, gets a well-formed message.
	client.pkt.SetPacketSize(minPacketSize)

	tok := parseLoginFailure(t, client.readReply(t))
	assert.Equal(t, errNumberLoginFailed, tok.Number)
}

func TestServerStartAndShutdown(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, config.MSSQLConfig{})
	assert.Nil(t, srv.Addr(), "no listener before Start")

	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Start("127.0.0.1:0")
	}()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 5*time.Second, 10*time.Millisecond)

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-errCh)
}

func TestNewServerRejectsAHalfConfiguredCertPair(t *testing.T) {
	t.Parallel()

	_, err := NewServer(nil, nil, config.DumpConfig{}, nil,
		config.MSSQLConfig{TLS: config.TLSConfig{CertFile: "/nope/cert.pem"}}, testLogger())
	require.ErrorIs(t, err, ErrTLSConfigInvalid)
}

func TestNewServerWithTLSDisabledHasNoTLSConfig(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}})
	assert.Nil(t, srv.tlsConfig)
}

func TestNextSPIDNeverReturnsZero(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}})

	srv.spidCounter.Store(0xFFFF)
	assert.NotZero(t, srv.nextSPID())
	assert.NotZero(t, srv.nextSPID())
}

func TestIsExpectedDisconnect(t *testing.T) {
	t.Parallel()

	assert.True(t, isExpectedDisconnect(io.EOF))
	assert.True(t, isExpectedDisconnect(net.ErrClosed))
	assert.False(t, isExpectedDisconnect(ErrExpectedLogin7))
}
