package postgresql

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
)

// makeSSLRequest builds the 8-byte PG SSLRequest preamble.
func makeSSLRequest() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], pgSSLRequestCode)
	return buf
}

// minimalSession constructs a Session wired with just the fields negotiateSSL
// touches — no store, cache, or logger required.
func minimalSession(conn net.Conn, tlsConfig *tls.Config) *Session {
	return &Session{
		clientConn:   conn,
		clientReader: bufio.NewReader(conn),
		ctx:          context.Background(),
		tlsConfig:    tlsConfig,
	}
}

func TestNegotiateSSL_DisabledRespondsN(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	defer func() { _ = serverSide.Close() }()
	defer func() { _ = clientSide.Close() }()

	go func() {
		_, _ = clientSide.Write(makeSSLRequest())
	}()

	s := minimalSession(serverSide, nil)

	doneCh := make(chan error, 1)
	go func() { doneCh <- s.negotiateSSL() }()

	resp := make([]byte, 1)
	if _, err := io.ReadFull(clientSide, resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp[0] != 'N' {
		t.Errorf("got %q, want 'N'", resp[0])
	}

	select {
	case err := <-doneCh:
		if err != nil {
			t.Errorf("negotiateSSL: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("negotiateSSL hung")
	}
}

func TestNegotiateSSL_EnabledUpgrades(t *testing.T) {
	t.Parallel()

	tlsConf, err := generateSelfSignedTLS()
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	clientSide, serverSide := net.Pipe()
	defer func() { _ = serverSide.Close() }()
	defer func() { _ = clientSide.Close() }()

	clientErrCh := make(chan error, 1)
	go func() {
		// Send SSLRequest, expect 'S', then run a TLS client handshake.
		if _, err := clientSide.Write(makeSSLRequest()); err != nil {
			clientErrCh <- err
			return
		}

		resp := make([]byte, 1)
		if _, err := io.ReadFull(clientSide, resp); err != nil {
			clientErrCh <- err
			return
		}
		if resp[0] != 'S' {
			clientErrCh <- io.ErrUnexpectedEOF
			return
		}

		clientTLS := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		clientErrCh <- clientTLS.Handshake()
	}()

	s := minimalSession(serverSide, tlsConf)

	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- s.negotiateSSL() }()

	select {
	case err := <-serverErrCh:
		if err != nil {
			t.Fatalf("server negotiateSSL: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server negotiateSSL hung")
	}

	select {
	case err := <-clientErrCh:
		if err != nil {
			t.Fatalf("client side: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client handshake hung")
	}

	// After upgrade, clientConn should be a *tls.Conn.
	if _, ok := s.clientConn.(*tls.Conn); !ok {
		t.Errorf("expected clientConn to be *tls.Conn after upgrade, got %T", s.clientConn)
	}
}

func TestNegotiateSSL_NoSSLRequestPassesThrough(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	defer func() { _ = serverSide.Close() }()
	defer func() { _ = clientSide.Close() }()

	// Build a regular StartupMessage shape: length=N (>8), version=196608 (3.0).
	// negotiateSSL should treat this as "not SSLRequest" and leave bytes in place.
	startup := make([]byte, 8)
	binary.BigEndian.PutUint32(startup[0:4], 32) // arbitrary length > 8
	binary.BigEndian.PutUint32(startup[4:8], 196608)

	go func() {
		_, _ = clientSide.Write(startup)
	}()

	s := minimalSession(serverSide, nil)

	doneCh := make(chan error, 1)
	go func() { doneCh <- s.negotiateSSL() }()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("negotiateSSL: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("negotiateSSL hung")
	}

	// The 8 bytes should still be available to read from clientReader.
	got, err := s.clientReader.Peek(8)
	if err != nil {
		t.Fatalf("peek after negotiateSSL: %v", err)
	}
	if string(got) != string(startup) {
		t.Errorf("startup bytes not preserved: got %x, want %x", got, startup)
	}
}

func TestLoadTLS_FromConfig_RoundTrip(t *testing.T) {
	t.Parallel()

	tlsConf, err := loadTLS(config.PGConfig{})
	if err != nil {
		t.Fatalf("loadTLS: %v", err)
	}
	if tlsConf == nil {
		t.Fatal("expected tlsConf non-nil")
	}
	if tlsConf.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want %d", tlsConf.MinVersion, tls.VersionTLS12)
	}
}
