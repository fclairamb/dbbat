package mssql

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

// fakeUpstream is a minimal TDS *server*: PRELOGIN, an optional encapsulated
// TLS handshake, LOGIN7, then a canned answer to everything else.
//
// It exists so the whole upstream leg — the ssl_mode plan, the redial chain,
// the LOGIN7 replay, the login-response classification and the relay — can be
// exercised under `make test` on any architecture. The real SQL Server image is
// linux/amd64 only, so the integration suite cannot be the first thing that
// tells you the connector is wrong.
type fakeUpstream struct {
	listener net.Listener

	// encryption is the ENCRYPTION byte answered in PRELOGIN.
	encryption byte
	// tlsConfig is used when the negotiation lands on a TLS handshake.
	tlsConfig *tls.Config
	// rejectLogin makes it answer LOGIN7 with a login failure.
	rejectLogin bool
	// batchResponse is what it answers every post-login message with.
	batchResponse []byte

	mu     sync.Mutex
	logins []*Login7
	// requests records every post-login message, so the relay can be proved to
	// have forwarded what the client actually sent.
	requests []fakeRequest
	// sawTLS records whether the last session ran a TLS handshake.
	sawTLS bool
}

// fakeRequest is one post-login message the fake was sent.
type fakeRequest struct {
	msgType byte
	payload []byte
}

// receivedRequests returns a copy of everything the fake was sent after login.
func (f *fakeUpstream) receivedRequests() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]fakeRequest(nil), f.requests...)
}

// newFakeUpstream starts a fake server on an ephemeral port.
func newFakeUpstream(t *testing.T, encryption byte) *fakeUpstream {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	tlsConfig, err := generateSelfSignedTLS()
	require.NoError(t, err)

	up := &fakeUpstream{
		listener:      listener,
		encryption:    encryption,
		tlsConfig:     tlsConfig,
		batchResponse: buildDoneToken(0, 1),
	}

	go up.acceptLoop()

	t.Cleanup(func() { _ = listener.Close() })

	return up
}

// addr is the host:port the fake listens on.
func (f *fakeUpstream) addr() (string, int) {
	tcpAddr, ok := f.listener.Addr().(*net.TCPAddr)
	if !ok {
		panic("fake upstream is not on TCP")
	}

	return tcpAddr.IP.String(), tcpAddr.Port
}

// dialFunc is the transport the connector is injected with — the same seam the
// SSH bastion chain plugs into in production.
func (f *fakeUpstream) dialFunc() upstream.DialFunc {
	host, port := f.addr()
	target := net.JoinHostPort(host, strconv.Itoa(port))

	return func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, "tcp", target)
	}
}

// lastLogin returns the most recent LOGIN7 the fake was sent.
func (f *fakeUpstream) lastLogin() *Login7 {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.logins) == 0 {
		return nil
	}

	return f.logins[len(f.logins)-1]
}

// loginCount is how many LOGIN7 messages the fake has seen, which is how a
// redial between ssl_mode attempts is observed.
func (f *fakeUpstream) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.logins)
}

// negotiatedTLS reports whether the last session ran a TLS handshake.
func (f *fakeUpstream) negotiatedTLS() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sawTLS
}

func (f *fakeUpstream) acceptLoop() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}

		go f.serve(conn)
	}
}

func (f *fakeUpstream) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	stream := newRevertibleConn(conn)
	pkt := newPacketRW(stream)

	login, ok := f.handshake(conn, stream, pkt)
	if !ok {
		return
	}

	if f.rejectLogin {
		_ = pkt.WriteMessage(packetTypeReply,
			buildLoginFailure(18456, "Login failed for user '"+login.UserName+"'."))

		return
	}

	pkt.SetPacketSize(int(login.PacketSize))

	if err := pkt.WriteMessage(packetTypeReply, buildAcceptedLoginResponse()); err != nil {
		return
	}

	// Deadlines are for the handshake only: the relay tests keep a session open
	// while they exchange messages.
	_ = conn.SetDeadline(time.Time{})

	for {
		msgType, payload, err := pkt.ReadMessage()
		if err != nil {
			return
		}

		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{msgType: msgType, payload: payload})
		response := f.batchResponse
		f.mu.Unlock()

		if err := pkt.WriteMessage(packetTypeReply, response); err != nil {
			return
		}
	}
}

// handshake runs PRELOGIN, the optional TLS handshake and LOGIN7.
func (f *fakeUpstream) handshake(conn net.Conn, stream *revertibleConn, pkt *packetRW) (*Login7, bool) {
	msgType, payload, err := pkt.ReadMessage()
	if err != nil || msgType != packetTypePrelogin {
		return nil, false
	}

	client, err := parsePrelogin(payload)
	if err != nil {
		return nil, false
	}

	if err := pkt.WriteMessage(packetTypeReply, buildPreloginResponse(client, f.encryption).serialize()); err != nil {
		return nil, false
	}

	encrypted := f.encryption == encryptOn || f.encryption == encryptReq

	f.mu.Lock()
	f.sawTLS = encrypted
	f.mu.Unlock()

	if encrypted {
		adapter := newHandshakeConn(conn, pkt)

		tlsConn := tls.Server(adapter, f.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return nil, false
		}

		if err := adapter.deactivate(); err != nil {
			return nil, false
		}

		stream.switchTo(tlsConn)
	}

	msgType, payload, err = pkt.ReadMessage()
	if err != nil || msgType != packetTypeLogin7 {
		return nil, false
	}

	login, err := parseLogin7(payload)
	if err != nil {
		return nil, false
	}

	f.mu.Lock()
	f.logins = append(f.logins, login)
	f.mu.Unlock()

	return login, true
}
