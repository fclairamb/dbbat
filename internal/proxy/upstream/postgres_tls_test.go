package upstream

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

var errSSLRequestMismatch = errors.New("SSLRequest preamble mismatch")

// readSSLRequest reads the 8-byte SSLRequest preamble from c and returns an
// error if it doesn't match what negotiatePostgresSSL is expected to send.
func readSSLRequest(c net.Conn) error {
	got := make([]byte, 8)
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	for i := range got {
		if got[i] != pgSSLRequest[i] {
			return errSSLRequestMismatch
		}
	}
	return nil
}

// negotiate runs the SSL negotiation for an ssl_mode against host "example.com",
// keeping the tests written in terms of the mode an operator actually types.
func negotiate(ctx context.Context, conn net.Conn, mode string) (net.Conn, bool, error) {
	return negotiatePostgresSSL(ctx, conn, PlanFor(mode, "example.com"))
}

func TestNegotiatePostgresSSL_DisableSkipsProbe(t *testing.T) {
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

	out, encrypted, err := negotiate(context.Background(), clientSide, SSLModeDisable)
	if err != nil {
		t.Fatalf("disable: unexpected error: %v", err)
	}
	if out != clientSide {
		t.Fatalf("disable: expected original conn returned, got different conn")
	}
	if encrypted {
		t.Fatalf("disable: reported an encrypted connection")
	}

	if err := <-probe; err == nil {
		t.Fatalf("disable: server received bytes, expected none")
	}
}

func TestNegotiatePostgresSSL_PreferFallsBackToPlaintext(t *testing.T) {
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

	out, encrypted, err := negotiate(context.Background(), clientSide, SSLModePrefer)
	if err != nil {
		t.Fatalf("prefer + N: unexpected error: %v", err)
	}
	if out != clientSide {
		t.Fatalf("prefer + N: expected plain conn passthrough")
	}
	if encrypted {
		t.Fatalf("prefer + N: reported an encrypted connection")
	}
}

func TestNegotiatePostgresSSL_RequireFailsOnDeny(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{SSLModeRequire, SSLModeVerifyCA, SSLModeVerifyFull} {
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

			_, _, err := negotiate(context.Background(), clientSide, mode)
			if !errors.Is(err, ErrPostgresTLSRequired) {
				t.Fatalf("%s + N: expected ErrPostgresTLSRequired, got %v", mode, err)
			}
		})
	}
}

func TestNegotiatePostgresSSL_UnexpectedResponseByte(t *testing.T) {
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

	_, _, err := negotiate(context.Background(), clientSide, SSLModePrefer)
	if !errors.Is(err, ErrPostgresSSLResponse) {
		t.Fatalf("expected ErrPostgresSSLResponse, got %v", err)
	}
}

func TestNegotiatePostgresSSL_AcceptUpgradesToTLS(t *testing.T) {
	t.Parallel()

	tlsConf := testServerTLS(t)

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

	out, encrypted, err := negotiate(context.Background(), clientSide, SSLModeRequire)
	if err != nil {
		t.Fatalf("require + S: handshake failed: %v", err)
	}
	if _, ok := out.(*tls.Conn); !ok {
		t.Fatalf("require + S: expected *tls.Conn, got %T", out)
	}
	if !encrypted {
		t.Fatalf("require + S: did not report an encrypted connection")
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

// testServerTLS builds a throwaway self-signed server config for the fake
// upstreams in this package's tests.
func testServerTLS(t *testing.T) *tls.Config {
	t.Helper()

	const rsaBits = 2048

	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dbbat-test-upstream"},
		DNSNames:     []string{"localhost", "example.com"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
	}
}
