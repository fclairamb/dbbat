package postgresql

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/conncheck"
	"github.com/fclairamb/dbbat/internal/store"
)

// sslWitness is a fake upstream that records, per connection, whether the first
// thing it received was an SSLRequest. It answers 'N' — "no TLS here" — and
// hangs up, which is enough to observe the negotiation without standing up a
// real PostgreSQL.
type sslWitness struct {
	mu sync.Mutex
	// seen records one entry per connection: true when that connection opened
	// with an SSLRequest rather than a StartupMessage.
	seen []bool
}

// start runs the witness on a fresh listener.
func (w *sslWitness) start(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}

			go w.serve(conn)
		}
	}()

	return ln
}

// serve reads the first 8 bytes of one connection and classifies them.
func (w *sslWitness) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}

	isSSL := binary.BigEndian.Uint32(header[0:4]) == 8 &&
		binary.BigEndian.Uint32(header[4:8]) == pgSSLRequestCode

	w.mu.Lock()
	w.seen = append(w.seen, isSSL)
	w.mu.Unlock()

	if isSSL {
		// Refuse TLS: an opportunistic client must carry on in plaintext.
		_, _ = conn.Write([]byte{'N'})
	}
}

// observed returns what the witness saw, oldest first.
func (w *sslWitness) observed() []bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]bool(nil), w.seen...)
}

// errResolverUnexpected marks a resolver call that should never happen: the
// rows under test have no via_uid, so the dialer has no bastion chain to walk.
var errResolverUnexpected = errors.New("resolver consulted for a direct dial")

// noResolver satisfies shared.ServerResolver for a row with no bastion, where
// the dialer never consults it. Any call means the test set up the wrong shape.
type noResolver struct{ t *testing.T }

func (r noResolver) GetServerByUID(context.Context, uuid.UUID) (*store.Server, error) {
	r.t.Error(errResolverUnexpected)

	return nil, errResolverUnexpected
}

func (r noResolver) SetKnownHostKey(context.Context, uuid.UUID, string) error {
	r.t.Error(errResolverUnexpected)

	return errResolverUnexpected
}

func (r noResolver) SetKubernetesCACert(context.Context, uuid.UUID, string) error {
	r.t.Error(errResolverUnexpected)

	return errResolverUnexpected
}

// witnessServer builds the server row both entry points are driven from.
func witnessServer(t *testing.T, ln net.Listener, sslMode string, key []byte) *store.Server {
	t.Helper()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", ln.Addr(), err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}

	uid := uuid.New()

	encrypted, err := crypto.Encrypt([]byte("s3cret"), key, crypto.ServerAAD(uid.String()))
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}

	return &store.Server{
		UID:               uid,
		Protocol:          store.ProtocolPostgreSQL,
		Host:              host,
		Port:              port,
		Username:          "app",
		PasswordEncrypted: encrypted,
		DatabaseName:      "app",
		SSLMode:           sslMode,
	}
}

// TestUpstreamConnect_ProxyAndProbeAgreeOnTheWire is the guard the whole
// unification exists for: the connectivity check and a real proxied session must
// put the *same bytes* on the wire for the same server row.
//
// They drifted before precisely because nothing checked this — the probe mapped
// ssl_mode=prefer to plaintext-only while the proxy sent an SSLRequest, so every
// target whose pg_hba.conf demands encryption reported "the database refused the
// stored credentials" for a server the proxy connects to fine.
//
// Both entry points are driven for real: Session.connectUpstream is what a live
// session runs, and conncheck.Checker.Check is the exported API the REST handler
// calls. Neither is stubbed. Since phase 3 they reach the same
// upstream.ConnectPostgres, so agreement is now structural — this test is what
// makes a future divergence fail loudly instead of silently.
func TestUpstreamConnect_ProxyAndProbeAgreeOnTheWire(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sslMode string
		wantSSL bool
	}{
		{name: "prefer offers TLS", sslMode: "prefer", wantSSL: true},
		{name: "empty defaults to prefer", sslMode: "", wantSSL: true},
		{name: "allow offers TLS", sslMode: "allow", wantSSL: true},
		{name: "disable stays plaintext", sslMode: "disable", wantSSL: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := make([]byte, 32)
			for i := range key {
				key[i] = byte(i + 1)
			}

			witness := &sslWitness{}
			ln := witness.start(t)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			// Entry point 1: a live proxied session. It is expected to fail —
			// the witness hangs up instead of authenticating — but only after
			// it has shown its hand.
			session := &Session{
				ctx:           ctx,
				encryptionKey: key,
				logger:        slog.New(slog.DiscardHandler),
				user:          &store.User{Username: "florent"},
				database:      witnessServer(t, ln, tc.sslMode, key),
			}
			_ = session.connectUpstream()

			// Entry point 2: the connectivity check, through its exported API.
			_ = conncheck.New(noResolver{t: t}, key).
				Check(ctx, witnessServer(t, ln, tc.sslMode, key))

			seen := witness.observed()
			if len(seen) != 2 {
				t.Fatalf("witness saw %d connections, want 2 (one per entry point)", len(seen))
			}

			if seen[0] != tc.wantSSL || seen[1] != tc.wantSSL {
				t.Fatalf("ssl_mode=%q: proxy sent SSLRequest=%v, probe sent SSLRequest=%v, want %v for both",
					tc.sslMode, seen[0], seen[1], tc.wantSSL)
			}
		})
	}
}
