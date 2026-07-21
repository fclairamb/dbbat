package conncheck

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/fclairamb/dbbat/internal/store"
)

var (
	errTestServerNotFound = errors.New("conncheck test: server not found")
	errTestUnknownKey     = errors.New("conncheck test: unknown key")
)

// fakeResolver is an in-memory shared.ServerResolver for the check tests.
type fakeResolver struct {
	mu      sync.Mutex
	servers map[uuid.UUID]*store.Server
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{servers: map[uuid.UUID]*store.Server{}}
}

func (f *fakeResolver) GetServerByUID(_ context.Context, uid uuid.UUID) (*store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	srv, ok := f.servers[uid]
	if !ok {
		return nil, errTestServerNotFound
	}

	// Deep-ish copy so decrypt-in-place on the copy does not race the map.
	cp := *srv
	if srv.ProtocolData != nil {
		pd := *srv.ProtocolData
		if pd.SSH != nil {
			sd := *pd.SSH
			pd.SSH = &sd
		}

		cp.ProtocolData = &pd
	}

	return &cp, nil
}

func (f *fakeResolver) SetKnownHostKey(_ context.Context, uid uuid.UUID, hostKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	srv, ok := f.servers[uid]
	if !ok {
		return errTestServerNotFound
	}

	if srv.ProtocolData == nil {
		srv.ProtocolData = &store.ServerProtocolData{}
	}

	if srv.ProtocolData.SSH == nil {
		srv.ProtocolData.SSH = &store.SSHServerData{}
	}

	srv.ProtocolData.SSH.KnownHostKey = hostKey

	return nil
}

// startFakeSSHServer boots an in-process SSH server accepting clientPub and
// forwarding direct-tcpip channels to a real dial.
func startFakeSSHServer(t *testing.T, clientPub ssh.PublicKey) net.Listener {
	t.Helper()

	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}

	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}

			return nil, errTestUnknownKey
		},
	}
	cfg.AddHostKey(hostSigner)

	go serveSSH(ln, cfg)
	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

func serveSSH(ln net.Listener, cfg *ssh.ServerConfig) {
	for {
		nConn, err := ln.Accept()
		if err != nil {
			return
		}

		go handleSSHConn(nConn, cfg)
	}
}

func handleSSHConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		_ = nConn.Close()

		return
	}

	defer func() { _ = sshConn.Close() }()

	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only direct-tcpip")

			continue
		}

		var payload struct {
			DestAddr string
			DestPort uint32
			OrigAddr string
			OrigPort uint32
		}

		if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
			_ = newCh.Reject(ssh.ConnectionFailed, "bad payload")

			continue
		}

		// Dial the destination *before* accepting, like a real sshd: a refused
		// target must surface as a channel rejection, not as a live channel that
		// immediately EOFs.
		upstream, err := net.Dial("tcp", net.JoinHostPort(payload.DestAddr, strconv.Itoa(int(payload.DestPort))))
		if err != nil {
			_ = newCh.Reject(ssh.ConnectionFailed, "dial failed")

			continue
		}

		ch, chReqs, err := newCh.Accept()
		if err != nil {
			_ = upstream.Close()

			continue
		}

		go ssh.DiscardRequests(chReqs)
		go pipeChannel(ch, upstream)
	}
}

func pipeChannel(ch ssh.Channel, upstream net.Conn) {
	go func() { _, _ = io.Copy(upstream, ch); _ = upstream.Close() }()
	go func() { _, _ = io.Copy(ch, upstream); _ = ch.Close() }()
}

// startEchoTarget starts a TCP echo server — a host that is reachable but does
// not speak any database protocol.
func startEchoTarget(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func(conn net.Conn) { _, _ = io.Copy(conn, conn); _ = conn.Close() }(c)
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

// startSilentTarget accepts connections and then never speaks — the shape of a
// host behind a black-holing middlebox.
func startSilentTarget(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("silent listen: %v", err)
	}

	go func() {
		var held []net.Conn

		for {
			c, err := ln.Accept()
			if err != nil {
				for _, h := range held {
					_ = h.Close()
				}

				return
			}

			held = append(held, c)
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

// closedPort returns an address nothing is listening on.
func closedPort(t *testing.T) (string, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := ln.Addr().String()
	_ = ln.Close()

	return splitHostPort(t, addr)
}

func genClientKey(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}

	return string(pem.EncodeToMemory(block)), signer.PublicKey()
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	return host, port
}

func testKey() []byte {
	return []byte("0123456789012345678901234567890X") // 32 bytes; unused on key-auth paths
}

// newBastion registers an ssh row in the resolver and returns it.
func newBastion(r *fakeResolver, host string, port int, privateKey string) *store.Server {
	uid := uuid.New()
	srv := &store.Server{
		UID: uid, Host: host, Port: port,
		Username: "tester", Protocol: store.ProtocolSSH,
		ProtocolData: &store.ServerProtocolData{SSH: &store.SSHServerData{PrivateKey: privateKey}},
	}
	r.servers[uid] = srv

	return srv
}

func TestCheck_BastionSuccessPinsHostKey(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, host, port, pk)

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if !res.OK {
		t.Fatalf("Check() ok = false, stage=%s code=%s msg=%s", res.Stage, res.Code, res.Message)
	}

	if res.Stage != StageBastionAuth || res.Code != CodeOK {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageBastionAuth, CodeOK)
	}

	if !res.HostKeyPinned {
		t.Error("HostKeyPinned = false, want true on first successful connect")
	}

	if !strings.HasPrefix(res.KnownHostKey, "ssh-rsa ") {
		t.Errorf("KnownHostKey = %q, want a pinned ssh-rsa key", res.KnownHostKey)
	}

	// A second check must not re-report a pin (the key is already known).
	res2 := New(resolver, testKey()).Check(context.Background(), resolver.servers[bastion.UID])
	if !res2.OK {
		t.Fatalf("second Check() ok = false: %s/%s %s", res2.Stage, res2.Code, res2.Message)
	}

	if res2.HostKeyPinned {
		t.Error("HostKeyPinned = true on the second check, want false")
	}
}

func TestCheck_BastionAuthRejected(t *testing.T) {
	t.Parallel()

	_, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	host, port := splitHostPort(t, ln.Addr().String())

	// A *different* key: valid PEM, but not the one the bastion accepts.
	otherKey, _ := genClientKey(t)

	resolver := newFakeResolver()
	bastion := newBastion(resolver, host, port, otherKey)

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want a rejection")
	}

	if res.Stage != StageBastionAuth || res.Code != CodeAuthRejected {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageBastionAuth, CodeAuthRejected)
	}
}

func TestCheck_BastionUnreachable(t *testing.T) {
	t.Parallel()

	pk, _ := genClientKey(t)
	host, port := closedPort(t)

	resolver := newFakeResolver()
	bastion := newBastion(resolver, host, port, pk)

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want unreachable")
	}

	if res.Stage != StageBastionDial || res.Code != CodeUnreachable {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageBastionDial, CodeUnreachable)
	}
}

func TestCheck_BastionDNSFailure(t *testing.T) {
	t.Parallel()

	pk, _ := genClientKey(t)

	resolver := newFakeResolver()
	bastion := newBastion(resolver, "dbbat-conncheck-does-not-exist.invalid", 22, pk)

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want a DNS failure")
	}

	if res.Stage != StageBastionDial || res.Code != CodeDNSFailure {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageBastionDial, CodeDNSFailure)
	}
}

func TestCheck_BastionHandshakeTimeout(t *testing.T) {
	t.Parallel()

	pk, _ := genClientKey(t)
	ln := startSilentTarget(t)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, host, port, pk)

	checker := New(resolver, testKey())
	checker.timeout = 500 * time.Millisecond

	start := time.Now()
	res := checker.Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want a timeout")
	}

	if res.Code != CodeTimeout {
		t.Errorf("code = %s, want %s (msg=%s)", res.Code, CodeTimeout, res.Message)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Check() took %s, want it bounded by the checker timeout", elapsed)
	}
}

func TestCheck_BastionHostKeyMismatch(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, host, port, pk)
	bastion.ProtocolData.SSH.KnownHostKey = "ssh-ed25519 AAAAWRONGKEY tampered"

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want a host-key mismatch")
	}

	if res.Stage != StageBastionAuth || res.Code != CodeHostKeyMismatch {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageBastionAuth, CodeHostKeyMismatch)
	}
}

func TestCheck_BastionNoAuthMethod(t *testing.T) {
	t.Parallel()

	ln := startFakeSSHServer(t, nil)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	uid := uuid.New()
	bastion := &store.Server{
		UID: uid, Host: host, Port: port,
		Username: "tester", Protocol: store.ProtocolSSH,
	}
	resolver.servers[uid] = bastion

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want a config failure")
	}

	if res.Stage != StageConfig || res.Code != CodeNoAuthMethod {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageConfig, CodeNoAuthMethod)
	}
}

func TestCheck_BastionBadPrivateKey(t *testing.T) {
	t.Parallel()

	ln := startFakeSSHServer(t, nil)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, host, port, "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-key\n-----END OPENSSH PRIVATE KEY-----\n")

	res := New(resolver, testKey()).Check(context.Background(), bastion)

	if res.OK {
		t.Fatal("Check() ok = true, want a bad-key failure")
	}

	if res.Stage != StageConfig || res.Code != CodeBadPrivateKey {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageConfig, CodeBadPrivateKey)
	}
}

func TestCheck_TargetThroughTunnel_DialFails(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	bastionHost, bastionPort := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, bastionHost, bastionPort, pk)

	deadHost, deadPort := closedPort(t)
	target := &store.Server{
		UID: uuid.New(), Host: deadHost, Port: deadPort,
		Protocol: store.ProtocolOracle, ViaUID: &bastion.UID,
		Username: "scott",
	}

	res := New(resolver, testKey()).Check(context.Background(), target)

	if res.OK {
		t.Fatal("Check() ok = true, want a target dial failure")
	}

	if res.Stage != StageTargetDial {
		t.Errorf("stage = %s, want %s (code=%s msg=%s)", res.Stage, StageTargetDial, res.Code, res.Message)
	}
}

func TestCheck_TargetThroughTunnel_NoProbeReachabilityOnly(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	bastionHost, bastionPort := splitHostPort(t, ln.Addr().String())

	echo := startEchoTarget(t)
	targetHost, targetPort := splitHostPort(t, echo.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, bastionHost, bastionPort, pk)

	target := &store.Server{
		UID: uuid.New(), Host: targetHost, Port: targetPort,
		Protocol: store.ProtocolOracle, ViaUID: &bastion.UID, Username: "scott",
	}

	res := New(resolver, testKey()).Check(context.Background(), target)

	if !res.OK {
		t.Fatalf("Check() ok = false: %s/%s %s", res.Stage, res.Code, res.Message)
	}

	if res.Stage != StageTargetDial || res.Code != CodeUnsupported {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageTargetDial, CodeUnsupported)
	}
}

func TestCheck_TargetThroughTunnel_ProtocolHandshakeFails(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	bastionHost, bastionPort := splitHostPort(t, ln.Addr().String())

	// An echo server is reachable but speaks no PostgreSQL: the tunnel works,
	// the database handshake does not — exactly the distinction the check exists
	// to draw.
	echo := startEchoTarget(t)
	targetHost, targetPort := splitHostPort(t, echo.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, bastionHost, bastionPort, pk)

	target := &store.Server{
		UID: uuid.New(), Host: targetHost, Port: targetPort,
		Protocol: store.ProtocolPostgreSQL, DatabaseName: "app", Username: "app",
		SSLMode: "disable", ViaUID: &bastion.UID,
	}

	checker := New(resolver, testKey())
	checker.timeout = 5 * time.Second

	res := checker.Check(context.Background(), target)

	if res.OK {
		t.Fatal("Check() ok = true, want a database handshake failure")
	}

	if res.Stage != StageTargetAuth {
		t.Errorf("stage = %s, want %s (code=%s msg=%s)", res.Stage, StageTargetAuth, res.Code, res.Message)
	}
}

func TestCheck_TargetDirect_Unreachable(t *testing.T) {
	t.Parallel()

	host, port := closedPort(t)

	target := &store.Server{
		UID: uuid.New(), Host: host, Port: port,
		Protocol: store.ProtocolPostgreSQL, DatabaseName: "app", Username: "app", SSLMode: "disable",
	}

	res := New(newFakeResolver(), testKey()).Check(context.Background(), target)

	if res.OK {
		t.Fatal("Check() ok = true, want unreachable")
	}

	if res.Stage != StageTargetDial || res.Code != CodeUnreachable {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageTargetDial, CodeUnreachable)
	}
}

// TestCheck_NeverLeaksSecrets is the security gate: whatever the outcome, the
// result must not carry private-key or passphrase material — it is rendered
// verbatim in the admin UI and written to the audit log.
func TestCheck_NeverLeaksSecrets(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	ln := startFakeSSHServer(t, pub)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()

	// One row that succeeds, one that fails auth, one that cannot be reached.
	good := newBastion(resolver, host, port, pk)

	otherKey, _ := genClientKey(t)
	bad := newBastion(resolver, host, port, otherKey)

	deadHost, deadPort := closedPort(t)
	unreachable := newBastion(resolver, deadHost, deadPort, pk)

	checker := New(resolver, testKey())

	for name, tc := range map[string]struct {
		srv    *store.Server
		secret string
	}{
		"success":     {good, pk},
		"auth failed": {bad, otherKey},
		"unreachable": {unreachable, pk},
	} {
		res := checker.Check(context.Background(), tc.srv)
		blob := string(res.Stage) + string(res.Code) + res.Message + res.KnownHostKey

		if strings.Contains(blob, "PRIVATE KEY") {
			t.Errorf("%s: result leaked PEM private-key material: %q", name, blob)
		}

		// Compare on the key body, ignoring PEM line wrapping.
		body := strings.ReplaceAll(tc.secret, "\n", "")
		if len(body) > 40 && strings.Contains(strings.ReplaceAll(blob, "\n", ""), body[20:60]) {
			t.Errorf("%s: result leaked private-key bytes", name)
		}
	}
}
