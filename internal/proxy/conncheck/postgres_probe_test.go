package conncheck

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

var errNoSSLRequest = errors.New("conncheck test: client did not send an SSLRequest")

// pgSSLRequestCode is the magic protocol version of the SSLRequest packet, the
// same constant the PostgreSQL proxy writes upstream.
const pgSSLRequestCode = 80877103

// sslProbeResult records what the fake target observed on the wire.
type sslProbeResult struct {
	sawSSLRequest bool
	err           error
}

// startPGSSLProbeTarget reads the first packet of every connection and reports
// what the *first* one was: an SSLRequest, or a StartupMessage. It answers 'N'
// ("no TLS here") so an opportunistic client falls back to plaintext, then
// hangs up so the probe fails fast instead of waiting on an authentication that
// will never come.
//
// It keeps accepting because an opportunistic ssl_mode makes two attempts, and
// a second attempt left dangling would stall the probe for its full timeout.
//
// That is enough to pin down the behavior under test — whether the probe even
// offers TLS — without a live PostgreSQL. Whether the *upgraded* connection
// then authenticates is the integration tests' job.
func startPGSSLProbeTarget(t *testing.T) (net.Listener, <-chan sslProbeResult) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pg listen: %v", err)
	}

	out := make(chan sslProbeResult, 1)

	report := func(res sslProbeResult) {
		select {
		case out <- res:
		default: // only the first attempt is the one under test
		}
	}

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				report(sslProbeResult{err: acceptErr})

				return
			}

			go servePGSSLProbe(conn, report)
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })

	return ln, out
}

// servePGSSLProbe handles one connection of the fake target.
func servePGSSLProbe(conn net.Conn, report func(sslProbeResult)) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		report(sslProbeResult{err: err})

		return
	}

	isSSL := binary.BigEndian.Uint32(header[0:4]) == 8 &&
		binary.BigEndian.Uint32(header[4:8]) == pgSSLRequestCode

	report(sslProbeResult{sawSSLRequest: isSSL})

	if isSSL {
		// Refuse TLS: an opportunistic client must carry on in plaintext.
		_, _ = conn.Write([]byte{'N'})
	}
}

// TestProbePostgres_PreferOffersTLS is the regression test for prefer-mode
// probes reporting a bogus "the database refused the stored credentials" on
// every target whose pg_hba.conf requires encryption: the probe used to skip
// the SSLRequest entirely, so the target rejected the plaintext attempt with
// SQLSTATE 28000 even though the proxy — which does negotiate — connects fine.
func TestProbePostgres_PreferOffersTLS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sslMode string
		wantSSL bool
	}{
		{name: "prefer negotiates", sslMode: "prefer", wantSSL: true},
		{name: "empty defaults to prefer", sslMode: "", wantSSL: true},
		{name: "require negotiates", sslMode: "require", wantSSL: true},
		{name: "disable stays plaintext", sslMode: "disable", wantSSL: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ln, results := startPGSSLProbeTarget(t)
			host, port := splitHostPort(t, ln.Addr().String())

			srv := &store.Server{
				Protocol:     store.ProtocolPostgreSQL,
				Host:         host,
				Port:         port,
				Username:     "app",
				Password:     "secret",
				DatabaseName: "app",
				SSLMode:      tc.sslMode,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// The probe is expected to fail: the fake target hangs up instead of
			// authenticating. What matters is what it put on the wire first.
			_ = probePostgres(ctx, srv, func(dialCtx context.Context) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(dialCtx, "tcp", ln.Addr().String())
			})

			select {
			case res := <-results:
				if res.err != nil {
					t.Fatalf("fake target: %v", res.err)
				}

				if res.sawSSLRequest != tc.wantSSL {
					t.Fatalf("ssl_mode=%q: sent SSLRequest=%v, want %v",
						tc.sslMode, res.sawSSLRequest, tc.wantSSL)
				}
			case <-time.After(5 * time.Second):
				t.Fatal(errNoSSLRequest)
			}
		})
	}
}

// TestPostgresTLSPlan pins the ssl_mode → (primary, fallback) mapping, which is
// where libpq parity actually lives.
func TestPostgresTLSPlan(t *testing.T) {
	t.Parallel()

	srv := &store.Server{Host: "db.example.com", Port: 5432}

	for _, tc := range []struct {
		name            string
		sslMode         string
		wantPrimaryTLS  bool
		wantFallbacks   int
		wantFallbackTLS bool
		wantServerName  string
	}{
		{name: "disable", sslMode: "disable", wantPrimaryTLS: false, wantFallbacks: 0},
		{name: "require", sslMode: "require", wantPrimaryTLS: true, wantFallbacks: 0},
		{
			name: "verify-full pins hostname", sslMode: "verify-full",
			wantPrimaryTLS: true, wantFallbacks: 0, wantServerName: "db.example.com",
		},
		{
			name: "verify-ca pins hostname", sslMode: "verify-ca",
			wantPrimaryTLS: true, wantFallbacks: 0, wantServerName: "db.example.com",
		},
		{
			name: "prefer tries TLS then plaintext", sslMode: "prefer",
			wantPrimaryTLS: true, wantFallbacks: 1, wantFallbackTLS: false,
		},
		{
			name: "empty tries TLS then plaintext", sslMode: "",
			wantPrimaryTLS: true, wantFallbacks: 1, wantFallbackTLS: false,
		},
		{
			name: "allow tries plaintext then TLS", sslMode: "allow",
			wantPrimaryTLS: false, wantFallbacks: 1, wantFallbackTLS: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := &store.Server{Host: srv.Host, Port: srv.Port, SSLMode: tc.sslMode}
			primary, fallbacks := postgresTLSPlan(srv)

			if (primary != nil) != tc.wantPrimaryTLS {
				t.Fatalf("primary TLS = %v, want %v", primary != nil, tc.wantPrimaryTLS)
			}

			if len(fallbacks) != tc.wantFallbacks {
				t.Fatalf("fallbacks = %d, want %d", len(fallbacks), tc.wantFallbacks)
			}

			if tc.wantFallbacks == 1 {
				fb := fallbacks[0]
				if (fb.TLSConfig != nil) != tc.wantFallbackTLS {
					t.Fatalf("fallback TLS = %v, want %v", fb.TLSConfig != nil, tc.wantFallbackTLS)
				}

				if fb.Host != srv.Host || fb.Port != uint16(srv.Port) {
					t.Fatalf("fallback target = %s:%d, want %s:%d", fb.Host, fb.Port, srv.Host, srv.Port)
				}
			}

			assertTLSExpectations(t, primary, tc.wantServerName)
		})
	}
}

// assertTLSExpectations checks the parts of a probe tls.Config that carry
// security meaning: the floor version, and whether the server is authenticated.
func assertTLSExpectations(t *testing.T, cfg *tls.Config, wantServerName string) {
	t.Helper()

	if cfg == nil {
		return
	}

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}

	if cfg.ServerName != wantServerName {
		t.Fatalf("ServerName = %q, want %q", cfg.ServerName, wantServerName)
	}

	// A pinned hostname means the chain must actually be verified.
	if wantServerName != "" && cfg.InsecureSkipVerify {
		t.Fatal("verify-ca/verify-full must not skip certificate verification")
	}
}
