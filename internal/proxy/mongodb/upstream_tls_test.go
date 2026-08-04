package mongodb

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

// tlsRecordHandshake is the first byte of a TLS ClientHello (content type
// "handshake"). No OP_MSG frame can start with it — a MongoDB message begins
// with a little-endian length — so it is enough to tell the two apart on a
// listener that must serve both.
const tlsRecordHandshake = 0x16

// fakeMongo is an upstream that speaks just enough MongoDB to observe how dbbat
// arrived: encrypted or not. It answers hello and then refuses the SASL
// exchange, because what is under test is the transport, not the credentials.
type fakeMongo struct {
	// serverTLS terminates TLS when set; when nil the fake behaves like a
	// mongod with TLS switched off and hangs up on a ClientHello.
	serverTLS *tls.Config

	mu sync.Mutex
	// attempts records, in order, whether each connection arrived encrypted.
	attempts []bool
}

// start runs the fake on a fresh listener.
func (f *fakeMongo) start(t *testing.T) net.Listener {
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

			go f.serve(conn)
		}
	}()

	return ln
}

// record notes one connection attempt's encryption state.
func (f *fakeMongo) record(encrypted bool) {
	f.mu.Lock()
	f.attempts = append(f.attempts, encrypted)
	f.mu.Unlock()
}

// observed returns the recorded attempts.
func (f *fakeMongo) observed() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]bool(nil), f.attempts...)
}

// serve handles one connection: sniff TLS, answer hello, refuse SASL.
func (f *fakeMongo) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	reader := bufio.NewReader(conn)

	first, err := reader.Peek(1)
	if err != nil {
		return
	}

	var live net.Conn = readerConn{Conn: conn, reader: reader}

	if first[0] == tlsRecordHandshake {
		if f.serverTLS == nil {
			// A mongod without TLS has nothing to say to a ClientHello.
			return
		}

		tlsConn := tls.Server(live, f.serverTLS)
		if err := tlsConn.Handshake(); err != nil {
			return
		}

		live = tlsConn

		f.record(true)
	} else {
		f.record(false)
	}

	f.exchange(live)
}

// exchange answers hello and then rejects the SASL start.
func (f *fakeMongo) exchange(conn net.Conn) {
	reader := bufio.NewReader(conn)

	reply := func(doc any) bool {
		out, err := buildOpMsgReply(1, 0, doc)
		if err != nil {
			return false
		}

		_, err = conn.Write(out)

		return err == nil
	}

	// hello
	if _, err := readMessage(reader); err != nil {
		return
	}

	if !reply(bson.D{{Key: "ok", Value: 1}, {Key: "helloOk", Value: true}}) {
		return
	}

	// saslStart — refused, which is an authentication answer and must never
	// downgrade the next attempt.
	if _, err := readMessage(reader); err != nil {
		return
	}

	reply(bson.D{{Key: "ok", Value: 0}, {Key: "errmsg", Value: "Authentication failed."}})
}

// readerConn lets the TLS server (or the plaintext path) consume the byte the
// sniffer peeked.
type readerConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c readerConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// TestConnectUpstream_OpportunisticTLS pins what an ssl_mode actually does to
// the MongoDB socket. MongoDB has no in-band negotiation, so "prefer" can only
// mean "try the TLS handshake, redial in plaintext if it fails" — and the
// assertion here is what arrived at the server, not merely that something
// connected.
func TestConnectUpstream_OpportunisticTLS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		serverTLS bool
		mode      string
		// wantAttempts is the encryption state of each connection the server
		// saw, in order. A refused ClientHello leaves no entry.
		wantAttempts []bool
	}{
		{
			name: "prefer encrypts against a TLS server", serverTLS: true,
			mode: upstream.SSLModePrefer, wantAttempts: []bool{true},
		},
		{
			name: "empty mode behaves as prefer", serverTLS: true,
			mode: "", wantAttempts: []bool{true},
		},
		{
			name: "prefer falls back on a plaintext server", serverTLS: false,
			mode: upstream.SSLModePrefer, wantAttempts: []bool{false},
		},
		{
			name: "allow stays plaintext when plaintext works", serverTLS: true,
			mode: upstream.SSLModeAllow, wantAttempts: []bool{false},
		},
		{
			name: "disable never offers TLS", serverTLS: true,
			mode: upstream.SSLModeDisable, wantAttempts: []bool{false},
		},
		{
			name: "require encrypts", serverTLS: true,
			mode: upstream.SSLModeRequire, wantAttempts: []bool{true},
		},
		{
			name: "require never downgrades", serverTLS: false,
			mode: upstream.SSLModeRequire, wantAttempts: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeMongo{}
			if tc.serverTLS {
				cfg, err := generateSelfSignedTLS()
				if err != nil {
					t.Fatalf("self-signed cert: %v", err)
				}

				fake.serverTLS = cfg
			}

			ln := fake.start(t)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			// Every case is expected to fail: the fake refuses the SASL
			// exchange. What is under test is how it was reached.
			up, err := ConnectUpstream(ctx, func(dialCtx context.Context) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(dialCtx, "tcp", ln.Addr().String())
			}, UpstreamConfig{
				Host:       "127.0.0.1",
				Username:   "app",
				Password:   "s3cret",
				AuthSource: "admin",
				AppName:    "dbbat test",
				SSLMode:    tc.mode,
			})
			if err == nil {
				_ = up.Close()

				t.Fatal("fake upstream refuses SASL; the connect should not have succeeded")
			}

			assertAttempts(t, fake.observed(), tc.wantAttempts)
		})
	}
}

// assertAttempts compares the encryption state of what the server saw.
func assertAttempts(t *testing.T, got, want []bool) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("server saw %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("server saw %v, want %v", got, want)
		}
	}
}
