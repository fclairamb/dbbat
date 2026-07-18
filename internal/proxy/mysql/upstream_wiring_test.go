package mysql

import (
	"errors"
	"net"
	"testing"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

// stubCommandHandler is a no-op gomysqlserver.Handler used by the fake MySQL
// servers in this file. Both fixtures below only need the connection
// handshake to complete (to exercise real CLIENT_CONNECT_ATTRS wire encoding
// and the SetAttributes call in applyUpstreamOptions); no command is ever
// dispatched, so every method here is unreachable in practice.
type stubCommandHandler struct{}

var errStubHandlerUnused = errors.New("stubCommandHandler: unexpected command in wiring test")

func (stubCommandHandler) UseDB(_ string) error { return nil }

func (stubCommandHandler) HandleQuery(_ string) (*gomysql.Result, error) {
	return nil, errStubHandlerUnused
}

func (stubCommandHandler) HandleFieldList(_, _ string) ([]*gomysql.Field, error) {
	return nil, errStubHandlerUnused
}

func (stubCommandHandler) HandleStmtPrepare(_ string) (int, int, any, error) {
	return 0, 0, nil, errStubHandlerUnused
}

func (stubCommandHandler) HandleStmtExecute(_ any, _ string, _ []any) (*gomysql.Result, error) {
	return nil, errStubHandlerUnused
}

func (stubCommandHandler) HandleStmtClose(_ any) error { return nil }

func (stubCommandHandler) HandleOtherCommand(_ byte, _ []byte) error {
	return errStubHandlerUnused
}

// serverConnResult carries the outcome of a background gomysqlserver.NewConn
// handshake back to the test goroutine that triggered it.
type serverConnResult struct {
	conn *gomysqlserver.Conn
	err  error
}

// acceptServerConn accepts a single connection on ln and drives it through
// the go-mysql server-side handshake, delivering the result on the returned
// channel once done. Must be called before the paired client dial below,
// since Accept blocks until a connection arrives.
//
// The credential is registered with mysql_native_password explicitly —
// matching gomysqlserver.NewDefaultServer's own defaultAuthMethod — rather
// than relying on the gomysqlserver.NewConn convenience wrapper, whose
// in-memory auth handler defaults to caching_sha2_password. That mismatch
// (greeting advertises native_password, credential expects caching_sha2)
// forces an AuthSwitchRequest round trip; readHandshakeResponse returns
// early on that path (server/handshake_resp.go: handleAuthMatch returns
// cont=false) and never reaches the CLIENT_CONNECT_ATTRS decode, silently
// dropping every connection attribute even though auth itself still
// succeeds. Matching the plugin up front keeps the handshake on the
// single-round-trip path that actually reads attributes.
func acceptServerConn(ln net.Listener, user, password string) <-chan serverConnResult {
	resultCh := make(chan serverConnResult, 1)

	go func() {
		netConn, err := ln.Accept()
		if err != nil {
			resultCh <- serverConnResult{nil, err}

			return
		}

		srv := gomysqlserver.NewDefaultServer()

		authHandler := gomysqlserver.NewInMemoryAuthenticationHandler(gomysql.AUTH_NATIVE_PASSWORD)
		if err := authHandler.AddUser(user, password, gomysql.AUTH_NATIVE_PASSWORD); err != nil {
			resultCh <- serverConnResult{nil, err}

			return
		}

		conn, err := srv.NewCustomizedConn(netConn, authHandler, stubCommandHandler{})
		resultCh <- serverConnResult{conn, err}
	}()

	return resultCh
}

// TestApplyUpstreamOptions_ProgramNameAttributeWiring exercises the actual
// wiring in applyUpstreamOptions (upstream.go), not just the pure
// buildUpstreamProgramName helper already covered by
// TestBuildUpstreamProgramName in upstream_test.go: reading the client's
// declared program_name off s.serverConn.Attributes(), composing it with
// s.user.Username, and calling c.SetAttributes on the upstream connection.
//
// It drives a real go-mysql client.Conn through applyUpstreamOptions as its
// connect Option — exactly as connectUpstream does in production — against a
// real go-mysql server.Conn standing in for the upstream database, so the
// attribute is round-tripped over the actual CLIENT_CONNECT_ATTRS wire
// encoding rather than asserted via reflection or a hand-built fake.
// s.serverConn itself is built the same way: a second real client/server
// handshake in which the fake "client" declares its own program_name — the
// $appName dbbat is supposed to intercept and fold into the upstream name.
func TestApplyUpstreamOptions_ProgramNameAttributeWiring(t *testing.T) {
	t.Parallel()

	const (
		dbbatUsername     = "florent"
		dbbatClientPass   = "clientpass"
		clientProgramName = "mysql-cli"
		upstreamUser      = "upstreamuser"
		upstreamPassword  = "upstreampass"
	)

	// --- Step 1: build s.serverConn, the client-facing connection dbbat
	// terminates, with a client-declared program_name attribute.
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (client-facing): %v", err)
	}
	defer func() { _ = clientLn.Close() }()

	serverConnCh := acceptServerConn(clientLn, dbbatUsername, dbbatClientPass)

	fakeClient, err := gomysqlclient.Connect(clientLn.Addr().String(), dbbatUsername, dbbatClientPass, "",
		func(c *gomysqlclient.Conn) error {
			c.SetAttributes(map[string]string{"program_name": clientProgramName})

			return nil
		})
	if err != nil {
		t.Fatalf("fake client connect (client-facing): %v", err)
	}
	defer func() { _ = fakeClient.Close() }()

	scRes := <-serverConnCh
	if scRes.err != nil {
		t.Fatalf("server-side handshake (client-facing): %v", scRes.err)
	}

	if got := scRes.conn.Attributes()["program_name"]; got != clientProgramName {
		t.Fatalf("sanity check failed: s.serverConn program_name = %q, want %q", got, clientProgramName)
	}

	// --- Step 2: build the Session under test and drive applyUpstreamOptions
	// through a real upstream-facing handshake.
	s := &Session{
		user:       &store.User{Username: dbbatUsername},
		database:   &store.Server{SSLMode: "disable"},
		serverConn: scRes.conn,
	}

	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (upstream): %v", err)
	}
	defer func() { _ = upstreamLn.Close() }()

	upstreamConnCh := acceptServerConn(upstreamLn, upstreamUser, upstreamPassword)

	upstreamClient, err := gomysqlclient.Connect(
		upstreamLn.Addr().String(), upstreamUser, upstreamPassword, "", s.applyUpstreamOptions)
	if err != nil {
		t.Fatalf("applyUpstreamOptions connect: %v", err)
	}
	defer func() { _ = upstreamClient.Close() }()

	ucRes := <-upstreamConnCh
	if ucRes.err != nil {
		t.Fatalf("server-side handshake (upstream): %v", ucRes.err)
	}

	want := "dbbat/" + version.Version + " @" + dbbatUsername + " for " + clientProgramName

	if got := ucRes.conn.Attributes()["program_name"]; got != want {
		t.Errorf("upstream program_name attribute = %q, want %q", got, want)
	}
}
