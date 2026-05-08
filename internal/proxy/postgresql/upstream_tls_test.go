package postgresql

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

var errSSLRequestMismatch = errors.New("SSLRequest preamble mismatch")

// readSSLRequest reads the 8-byte SSLRequest preamble from c and returns an
// error if it doesn't match what negotiateUpstreamSSL is expected to send.
func readSSLRequest(c net.Conn) error {
	got := make([]byte, 8)
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	for i := range got {
		if got[i] != upstreamSSLRequest[i] {
			return errSSLRequestMismatch
		}
	}
	return nil
}

func TestNegotiateUpstreamSSL_DisableSkipsProbe(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	defer func() { _ = serverSide.Close() }()

	// Server reads nothing; if anything is sent, the test will hang and
	// time out. Use a short read with deadline to assert silence.
	probe := make(chan error, 1)
	go func() {
		_ = serverSide.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 1)
		_, err := serverSide.Read(buf)
		probe <- err
	}()

	out, err := negotiateUpstreamSSL(context.Background(), clientSide, "example.com", "disable")
	if err != nil {
		t.Fatalf("disable: unexpected error: %v", err)
	}
	if out != clientSide {
		t.Fatalf("disable: expected original conn returned, got different conn")
	}

	if err := <-probe; err == nil {
		t.Fatalf("disable: server received bytes, expected none")
	}
}

func TestNegotiateUpstreamSSL_PreferFallsBackToPlaintext(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	defer func() { _ = serverSide.Close() }()

	go func() {
		if err := readSSLRequest(serverSide); err != nil {
			return
		}
		_, _ = serverSide.Write([]byte{'N'})
	}()

	out, err := negotiateUpstreamSSL(context.Background(), clientSide, "example.com", "prefer")
	if err != nil {
		t.Fatalf("prefer + N: unexpected error: %v", err)
	}
	if out != clientSide {
		t.Fatalf("prefer + N: expected plain conn passthrough")
	}
}

func TestNegotiateUpstreamSSL_RequireFailsOnDeny(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			clientSide, serverSide := net.Pipe()
			defer func() { _ = clientSide.Close() }()
			defer func() { _ = serverSide.Close() }()

			go func() {
				if err := readSSLRequest(serverSide); err != nil {
					return
				}
				_, _ = serverSide.Write([]byte{'N'})
			}()

			_, err := negotiateUpstreamSSL(context.Background(), clientSide, "example.com", mode)
			if !errors.Is(err, ErrUpstreamTLSRequired) {
				t.Fatalf("%s + N: expected ErrUpstreamTLSRequired, got %v", mode, err)
			}
		})
	}
}

func TestNegotiateUpstreamSSL_UnexpectedResponseByte(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	defer func() { _ = serverSide.Close() }()

	go func() {
		if err := readSSLRequest(serverSide); err != nil {
			return
		}
		_, _ = serverSide.Write([]byte{'X'})
	}()

	_, err := negotiateUpstreamSSL(context.Background(), clientSide, "example.com", "prefer")
	if !errors.Is(err, ErrUpstreamSSLResponse) {
		t.Fatalf("expected ErrUpstreamSSLResponse, got %v", err)
	}
}

func TestNegotiateUpstreamSSL_AcceptUpgradesToTLS(t *testing.T) {
	t.Parallel()

	tlsConf, err := generateSelfSignedTLS()
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()

	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverSide.Close() }()
		if err := readSSLRequest(serverSide); err != nil {
			serverErr <- err
			return
		}
		if _, err := serverSide.Write([]byte{'S'}); err != nil {
			serverErr <- err
			return
		}
		tlsConn := tls.Server(serverSide, tlsConf)
		serverErr <- tlsConn.Handshake()
	}()

	out, err := negotiateUpstreamSSL(context.Background(), clientSide, "example.com", "require")
	if err != nil {
		t.Fatalf("require + S: handshake failed: %v", err)
	}
	if _, ok := out.(*tls.Conn); !ok {
		t.Fatalf("require + S: expected *tls.Conn, got %T", out)
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server side: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server handshake hung")
	}
}

func TestUpstreamTLSConfig_ServerNameSetForVerifyModes(t *testing.T) {
	t.Parallel()

	cfg := upstreamTLSConfig("example.com", "verify-full")
	if cfg.ServerName != "example.com" || cfg.InsecureSkipVerify {
		t.Fatalf("verify-full: ServerName=%q InsecureSkipVerify=%v", cfg.ServerName, cfg.InsecureSkipVerify)
	}

	cfg = upstreamTLSConfig("example.com", "verify-ca")
	if cfg.ServerName != "example.com" || cfg.InsecureSkipVerify {
		t.Fatalf("verify-ca: ServerName=%q InsecureSkipVerify=%v", cfg.ServerName, cfg.InsecureSkipVerify)
	}

	cfg = upstreamTLSConfig("example.com", "require")
	if !cfg.InsecureSkipVerify {
		t.Fatalf("require: expected InsecureSkipVerify=true")
	}
}
