package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"
)

const (
	fakeMySQLUser     = "app"
	fakeMySQLPassword = "s3cret"
)

var errFakeMySQLCommand = errors.New("fake mysql: unexpected command")

// fakeMySQLHandler answers nothing: the tests only need the handshake.
type fakeMySQLHandler struct{}

func (fakeMySQLHandler) UseDB(_ string) error { return nil }

func (fakeMySQLHandler) HandleQuery(_ string) (*gomysql.Result, error) {
	return nil, errFakeMySQLCommand
}

func (fakeMySQLHandler) HandleFieldList(_ string, _ string) ([]*gomysql.Field, error) {
	return nil, errFakeMySQLCommand
}

func (fakeMySQLHandler) HandleStmtPrepare(_ string) (int, int, any, error) {
	return 0, 0, nil, errFakeMySQLCommand
}

func (fakeMySQLHandler) HandleStmtExecute(_ any, _ string, _ []any) (*gomysql.Result, error) {
	return nil, errFakeMySQLCommand
}

func (fakeMySQLHandler) HandleStmtClose(_ any) error { return nil }

func (fakeMySQLHandler) HandleOtherCommand(_ byte, _ []byte) error {
	return errFakeMySQLCommand
}

// startFakeMySQL runs a go-mysql server that either advertises CLIENT_SSL (and
// terminates TLS) or does not, which is exactly the distinction an
// opportunistic ssl_mode has to cope with.
func startFakeMySQL(t *testing.T, withTLS bool) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	// Credentials are registered with mysql_native_password to match the
	// greeting's defaultAuthMethod; a mismatch forces an AuthSwitchRequest
	// round trip that never reaches the attribute decode.
	authHandler := gomysqlserver.NewInMemoryAuthenticationHandler(gomysql.AUTH_NATIVE_PASSWORD)
	if err := authHandler.AddUser(fakeMySQLUser, fakeMySQLPassword, gomysql.AUTH_NATIVE_PASSWORD); err != nil {
		t.Fatalf("add user: %v", err)
	}

	var serverTLS *tls.Config
	if withTLS {
		serverTLS = testServerTLS(t)
	}

	srv := gomysqlserver.NewServer("8.0.0", gomysql.DEFAULT_COLLATION_ID,
		gomysql.AUTH_NATIVE_PASSWORD, nil, serverTLS)

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

				c, connErr := srv.NewCustomizedConn(conn, authHandler, fakeMySQLHandler{})
				if connErr != nil {
					_ = conn.Close()

					return
				}

				// Hold the connection open; the tests close from their side.
				_ = c.SetDeadline(time.Time{})
			}()
		}
	}()

	return ln
}

// connectFakeMySQL runs ConnectMySQL against ln at the given ssl_mode.
func connectFakeMySQL(t *testing.T, ln net.Listener, mode string) (*MySQLUpstream, error) {
	t.Helper()

	host, port := splitHostPortForTest(t, ln.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return ConnectMySQL(ctx, func(dialCtx context.Context) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(dialCtx, "tcp", ln.Addr().String())
	}, MySQLConfig{
		Host:        host,
		Port:        port,
		Username:    fakeMySQLUser,
		Password:    fakeMySQLPassword,
		ProgramName: "dbbat test",
		SSLMode:     mode,
	})
}

// TestConnectMySQL_OpportunisticTLS is the point of the whole exercise on the
// MySQL side: under prefer, dbbat must actually encrypt when the server can,
// and must still connect when it cannot. The assertion is the encryption state
// of the socket, not merely that the connection succeeded.
func TestConnectMySQL_OpportunisticTLS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		serverTLS  bool
		mode       string
		wantTLS    bool
		wantFailed bool
	}{
		{name: "prefer encrypts against a TLS server", serverTLS: true, mode: SSLModePrefer, wantTLS: true},
		{name: "prefer falls back on a plaintext server", serverTLS: false, mode: SSLModePrefer, wantTLS: false},
		{name: "empty mode behaves as prefer", serverTLS: true, mode: "", wantTLS: true},
		{name: "allow stays plaintext when plaintext works", serverTLS: true, mode: SSLModeAllow, wantTLS: false},
		{name: "disable stays plaintext against a TLS server", serverTLS: true, mode: SSLModeDisable, wantTLS: false},
		{name: "require encrypts", serverTLS: true, mode: SSLModeRequire, wantTLS: true},
		{name: "require fails on a plaintext server", serverTLS: false, mode: SSLModeRequire, wantFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ln := startFakeMySQL(t, tc.serverTLS)

			up, err := connectFakeMySQL(t, ln, tc.mode)

			if tc.wantFailed {
				if err == nil {
					_ = up.Close()

					t.Fatal("expected the connection to fail, it succeeded")
				}

				return
			}

			if err != nil {
				t.Fatalf("ConnectMySQL(ssl_mode=%q): %v", tc.mode, err)
			}

			defer func() { _ = up.Close() }()

			if up.TLS != tc.wantTLS {
				t.Fatalf("ssl_mode=%q against tls=%v: encrypted=%v, want %v",
					tc.mode, tc.serverTLS, up.TLS, tc.wantTLS)
			}

			assertSocketEncryption(t, up, tc.wantTLS)
		})
	}
}

// TestConnectMySQL_AuthFailureEndsTheChain is the libpq/pgconn rule the
// opportunistic retry must obey: only a transport problem may downgrade the
// next attempt. A rejected password is the server's answer about us, and
// retrying it in plaintext would be a downgrade triggered by the wrong signal.
func TestConnectMySQL_AuthFailureEndsTheChain(t *testing.T) {
	t.Parallel()

	ln := startFakeMySQL(t, true)
	host, port := splitHostPortForTest(t, ln.Addr().String())

	var dials int

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := ConnectMySQL(ctx, func(dialCtx context.Context) (net.Conn, error) {
		dials++

		var d net.Dialer

		return d.DialContext(dialCtx, "tcp", ln.Addr().String())
	}, MySQLConfig{
		Host:     host,
		Port:     port,
		Username: fakeMySQLUser,
		Password: "not-the-password",
		SSLMode:  SSLModePrefer,
	})
	if err == nil {
		t.Fatal("expected the wrong password to be refused")
	}

	if dials != 1 {
		t.Fatalf("dialed %d times, want 1: an auth failure must not trigger a plaintext retry", dials)
	}
}

// assertSocketEncryption checks the claim against the actual connection, so a
// bookkeeping bug cannot make an unencrypted socket look encrypted.
func assertSocketEncryption(t *testing.T, up *MySQLUpstream, wantTLS bool) {
	t.Helper()

	_, isTLS := up.Conn.Conn.Conn.(*tls.Conn)
	if isTLS != wantTLS {
		t.Fatalf("underlying socket is *tls.Conn=%v, want %v", isTLS, wantTLS)
	}
}

// splitHostPortForTest turns a listener address into the host/port pair a
// server row carries.
func splitHostPortForTest(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}

	return host, port
}
