package upstream

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

var (
	errWrongPassword  = errors.New("fake upstream: wrong password")
	errNotAPassword   = errors.New("fake upstream: expected a PasswordMessage")
	errStartupMissing = errors.New("fake upstream: startup parameter missing")
)

// fakePostgres is a minimal upstream that speaks just enough of the protocol to
// drive ConnectPostgres: cleartext password auth, then the startup state a real
// server sends before ReadyForQuery.
type fakePostgres struct {
	// password is what the fake accepts; anything else gets an ErrorResponse.
	password string
	// wantApplicationName, when set, is asserted against the StartupMessage.
	wantApplicationName string
	// rejectWith, when set, is the ErrorResponse message sent instead of
	// completing the login.
	rejectWith string
}

// serve runs the fake against one connection and reports what went wrong, if
// anything, on the returned channel.
func (f fakePostgres) serve(t *testing.T) (*net.TCPAddr, <-chan error) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan error, 1)

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			done <- acceptErr

			return
		}

		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		done <- f.handshake(conn)
	}()

	addr, _ := ln.Addr().(*net.TCPAddr)

	return addr, done
}

// handshake performs the server half of the login.
func (f fakePostgres) handshake(conn net.Conn) error {
	backend := pgproto3.NewBackend(conn, conn)

	startup, err := backend.ReceiveStartupMessage()
	if err != nil {
		return err
	}

	msg, ok := startup.(*pgproto3.StartupMessage)
	if !ok {
		return errStartupMissing
	}

	if f.wantApplicationName != "" && msg.Parameters["application_name"] != f.wantApplicationName {
		return errStartupMissing
	}

	if f.rejectWith != "" {
		return send(backend, conn, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: f.rejectWith})
	}

	if err := send(backend, conn, &pgproto3.AuthenticationCleartextPassword{}); err != nil {
		return err
	}

	pwMsg, err := backend.Receive()
	if err != nil {
		return err
	}

	pw, ok := pwMsg.(*pgproto3.PasswordMessage)
	if !ok {
		return errNotAPassword
	}

	if pw.Password != f.password {
		return errWrongPassword
	}

	for _, out := range []pgproto3.BackendMessage{
		&pgproto3.AuthenticationOk{},
		&pgproto3.ParameterStatus{Name: "server_version", Value: "16.2"},
		&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"},
		&pgproto3.BackendKeyData{ProcessID: 4242, SecretKey: []byte{1, 2, 3, 4}},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	} {
		if err := send(backend, conn, out); err != nil {
			return err
		}
	}

	return nil
}

// send writes one backend message and flushes it.
func send(backend *pgproto3.Backend, _ net.Conn, msg pgproto3.BackendMessage) error {
	backend.Send(msg)

	return backend.Flush()
}

// dialTo builds a DialFunc pointed at addr.
func dialTo(addr net.Addr) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "tcp", addr.String())
	}
}

// TestConnectPostgres_CapturesStartupState is the contract the proxy depends on:
// the connector stops at ReadyForQuery and hands back everything the login
// produced, so the session can replay it to its own client without owning a
// second copy of the login.
func TestConnectPostgres_CapturesStartupState(t *testing.T) {
	t.Parallel()

	const appName = "dbbat/test @alice"

	fake := fakePostgres{password: "s3cret", wantApplicationName: appName}
	addr, served := fake.serve(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	up, err := ConnectPostgres(ctx, dialTo(addr), PostgresConfig{
		Host:            "127.0.0.1",
		Username:        "alice",
		Password:        "s3cret",
		Database:        "app",
		ApplicationName: appName,
		SSLMode:         SSLModeDisable,
	}, nil)
	if err != nil {
		t.Fatalf("ConnectPostgres: %v", err)
	}

	defer func() { _ = up.Close() }()

	if len(up.ParameterStatuses) != 2 {
		t.Fatalf("ParameterStatuses = %d, want 2", len(up.ParameterStatuses))
	}

	// Copies, not aliases into pgproto3's reusable buffer: the two must differ.
	if up.ParameterStatuses[0].Name == up.ParameterStatuses[1].Name {
		t.Fatal("ParameterStatus messages were not copied out of the decoder's buffer")
	}

	if up.BackendKeyData == nil || up.BackendKeyData.ProcessID != 4242 {
		t.Fatalf("BackendKeyData = %+v, want ProcessID 4242", up.BackendKeyData)
	}

	if up.ReadyForQuery == nil || up.ReadyForQuery.TxStatus != 'I' {
		t.Fatalf("ReadyForQuery = %+v, want TxStatus 'I'", up.ReadyForQuery)
	}

	if up.TLS {
		t.Fatal("ssl_mode=disable reported an encrypted connection")
	}

	if err := <-served; err != nil {
		t.Fatalf("fake upstream: %v", err)
	}
}

// TestConnectPostgres_SurfacesErrorResponse pins the shape the proxy needs to
// forward the server's own wording to its client, and the classification the
// connectivity check needs to call it an auth failure rather than a network one.
func TestConnectPostgres_SurfacesErrorResponse(t *testing.T) {
	t.Parallel()

	const msg = `password authentication failed for user "alice"`

	fake := fakePostgres{password: "s3cret", rejectWith: msg}
	addr, served := fake.serve(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := ConnectPostgres(ctx, dialTo(addr), PostgresConfig{
		Host:     "127.0.0.1",
		Username: "alice",
		Password: "wrong",
		Database: "app",
		SSLMode:  SSLModeDisable,
	}, nil)

	if !errors.Is(err, ErrPostgresAuthFailed) {
		t.Fatalf("err = %v, want ErrPostgresAuthFailed", err)
	}

	var authErr *PostgresAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want a *PostgresAuthError carrying the raw response", err)
	}

	if authErr.Response.Message != msg {
		t.Fatalf("Response.Message = %q, want %q", authErr.Response.Message, msg)
	}

	if err := <-served; err != nil {
		t.Fatalf("fake upstream: %v", err)
	}
}
