package conncheck

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

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

// The ssl_mode → attempt-plan mapping used to be pinned here, against the
// probe's own pgconn translation of it. That translation is gone: the probe
// calls the proxy's connector, and the policy is described once by
// upstream.TestPlanFor. What stays in this file is the wire-level assertion
// that a prefer-mode probe really offers TLS, plus the classification below.

// startPGRejectingTarget accepts one connection, refuses TLS if offered, then
// answers the StartupMessage with a FATAL ErrorResponse — what a real Postgres
// sends when the credentials are wrong.
func startPGRejectingTarget(t *testing.T, message string) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pg listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		backend := pgproto3.NewBackend(conn, conn)

		for {
			msg, recvErr := backend.ReceiveStartupMessage()
			if recvErr != nil {
				return
			}

			if _, isSSL := msg.(*pgproto3.SSLRequest); isSSL {
				if _, writeErr := conn.Write([]byte{'N'}); writeErr != nil {
					return
				}

				continue
			}

			backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: message})
			_ = backend.Flush()

			return
		}
	}()

	return ln
}

// TestProbePostgres_ClassifiesFailures is the guard on the half of the probe
// that is genuinely probe-specific and must survive the move onto the proxy's
// connector: the stage/code mapping the UI keys its guidance off. A refused
// password and a refused TLS upgrade point at different fields, so they must
// not collapse into one code.
func TestProbePostgres_ClassifiesFailures(t *testing.T) {
	t.Parallel()

	t.Run("rejected credentials are an auth failure", func(t *testing.T) {
		t.Parallel()

		ln := startPGRejectingTarget(t, `password authentication failed for user "app"`)
		host, port := splitHostPort(t, ln.Addr().String())

		res := runPGProbe(t, &store.Server{
			Protocol: store.ProtocolPostgreSQL, Host: host, Port: port,
			Username: "app", Password: "secret", DatabaseName: "app", SSLMode: "prefer",
		}, ln)

		if res.Stage != StageTargetAuth || res.Code != CodeDBAuthFailed {
			t.Fatalf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageTargetAuth, CodeDBAuthFailed)
		}
	})

	t.Run("refused TLS is a handshake failure", func(t *testing.T) {
		t.Parallel()

		// Same target, but ssl_mode=require: it answers 'N', which is a
		// configuration problem on the target, not a credentials problem.
		ln := startPGRejectingTarget(t, `password authentication failed for user "app"`)
		host, port := splitHostPort(t, ln.Addr().String())

		res := runPGProbe(t, &store.Server{
			Protocol: store.ProtocolPostgreSQL, Host: host, Port: port,
			Username: "app", Password: "secret", DatabaseName: "app", SSLMode: "require",
		}, ln)

		if res.Stage != StageTargetAuth || res.Code != CodeDBHandshakeFailed {
			t.Fatalf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageTargetAuth, CodeDBHandshakeFailed)
		}
	})
}

// runPGProbe drives probePostgres against ln and classifies the failure the way
// the checker does.
func runPGProbe(t *testing.T, srv *store.Server, ln net.Listener) Result {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := probePostgres(ctx, srv, func(dialCtx context.Context) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(dialCtx, "tcp", ln.Addr().String())
	})
	if err == nil {
		t.Fatal("probe unexpectedly succeeded against a rejecting target")
	}

	return classifyTargetError(err)
}
