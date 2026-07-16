package shared

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/fclairamb/dbbat/internal/store"
)

var (
	errTestServerNotFound = errors.New("test: server not found")
	errTestUnknownKey     = errors.New("test: unknown key")
)

// fakeResolver is an in-memory ServerResolver for the dialer tests.
type fakeResolver struct {
	mu       sync.Mutex
	servers  map[uuid.UUID]*store.Server
	hostKeys map[uuid.UUID]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		servers:  map[uuid.UUID]*store.Server{},
		hostKeys: map[uuid.UUID]string{},
	}
}

func (f *fakeResolver) GetServerByUID(_ context.Context, uid uuid.UUID) (*store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	srv, ok := f.servers[uid]
	if !ok {
		return nil, errTestServerNotFound
	}
	// Return a shallow copy so callers mutating decrypted fields don't race the map.
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
	f.hostKeys[uid] = hostKey
	if srv, ok := f.servers[uid]; ok {
		if srv.ProtocolData == nil {
			srv.ProtocolData = &store.ServerProtocolData{}
		}
		if srv.ProtocolData.SSH == nil {
			srv.ProtocolData.SSH = &store.SSHServerData{}
		}
		srv.ProtocolData.SSH.KnownHostKey = hostKey
	}
	return nil
}

// fakeSSHServer is an in-process SSH server that accepts direct-tcpip channels
// and pipes them to a local echo/target listener.
type fakeSSHServer struct {
	listener   net.Listener
	hostSigner ssh.Signer
	clientAuth ssh.PublicKey
	dialCount  int
	mu         sync.Mutex
	closed     chan struct{}
}

// startFakeSSHServer boots a fake SSH server authenticating the given client
// public key. It forwards direct-tcpip requests to a real net.Dial.
func startFakeSSHServer(t *testing.T, clientPub ssh.PublicKey) *fakeSSHServer {
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

	s := &fakeSSHServer{
		listener:   ln,
		hostSigner: hostSigner,
		clientAuth: clientPub,
		closed:     make(chan struct{}),
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

	go s.serve(cfg)
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSSHServer) addr() string { return s.listener.Addr().String() }

func (s *fakeSSHServer) serve(cfg *ssh.ServerConfig) {
	for {
		nConn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(nConn, cfg)
	}
}

func (s *fakeSSHServer) handleConn(nConn net.Conn, cfg *ssh.ServerConfig) {
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
		s.mu.Lock()
		s.dialCount++
		s.mu.Unlock()

		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(chReqs)
		// Parse the direct-tcpip payload to get the destination.
		var payload struct {
			DestAddr string
			DestPort uint32
			OrigAddr string
			OrigPort uint32
		}
		if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
			_ = ch.Close()
			continue
		}
		target := net.JoinHostPort(payload.DestAddr, strconv.Itoa(int(payload.DestPort)))
		go s.pipe(ch, target)
	}
}

func (s *fakeSSHServer) pipe(ch ssh.Channel, target string) {
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_ = ch.Close()
		return
	}
	go func() { _, _ = io.Copy(upstream, ch); _ = upstream.Close() }()
	go func() { _, _ = io.Copy(ch, upstream); _ = ch.Close() }()
}

// startEchoTarget starts a TCP server that echoes back everything it reads.
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

// genClientKey returns a fresh RSA private key (PEM) and its ssh.PublicKey.
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
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	return host, p
}

func TestDialUpstream_Direct(t *testing.T) {
	t.Parallel()
	echo := startEchoTarget(t)
	host, port := splitHostPort(t, echo.Addr().String())

	d := NewDialer()
	srv := &store.Server{Host: host, Port: port, Protocol: store.ProtocolPostgreSQL}
	conn, err := d.DialUpstream(context.Background(), newFakeResolver(), testKey(), srv)
	if err != nil {
		t.Fatalf("DialUpstream(direct) error = %v", err)
	}
	defer func() { _ = conn.Close() }()
	assertEcho(t, conn)
}

func TestDialUpstream_ThroughSSHBastion(t *testing.T) {
	t.Parallel()
	echo := startEchoTarget(t)
	targetHost, targetPort := splitHostPort(t, echo.Addr().String())

	pk, pub := genClientKey(t)
	fake := startFakeSSHServer(t, pub)
	bastionHost, bastionPort := splitHostPort(t, fake.addr())

	resolver := newFakeResolver()
	bastionUID := uuid.New()
	resolver.servers[bastionUID] = &store.Server{
		UID: bastionUID, Host: bastionHost, Port: bastionPort,
		Username: "tester", Protocol: store.ProtocolSSH,
		ProtocolData: &store.ServerProtocolData{SSH: &store.SSHServerData{PrivateKey: pk}},
	}

	target := &store.Server{
		Host: targetHost, Port: targetPort, Protocol: store.ProtocolPostgreSQL,
		ViaUID: &bastionUID,
	}

	d := NewDialer()
	conn, err := d.DialUpstream(context.Background(), resolver, testKey(), target)
	if err != nil {
		t.Fatalf("DialUpstream(ssh) error = %v", err)
	}
	defer func() { _ = conn.Close() }()
	assertEcho(t, conn)

	// TOFU: the host key must have been recorded.
	resolver.mu.Lock()
	recorded := resolver.hostKeys[bastionUID]
	resolver.mu.Unlock()
	if recorded == "" {
		t.Error("host key was not pinned on first connect (TOFU)")
	}

	// Pooling: a second dial must reuse the same ssh.Client (no new handshake).
	conn2, err := d.DialUpstream(context.Background(), resolver, testKey(), target)
	if err != nil {
		t.Fatalf("DialUpstream(ssh) second error = %v", err)
	}
	defer func() { _ = conn2.Close() }()

	d.mu.Lock()
	poolSize := len(d.clients)
	d.mu.Unlock()
	if poolSize != 1 {
		t.Errorf("pool size = %d, want 1 (client should be reused)", poolSize)
	}
}

func TestDialUpstream_HostKeyMismatch(t *testing.T) {
	t.Parallel()
	echo := startEchoTarget(t)
	targetHost, targetPort := splitHostPort(t, echo.Addr().String())

	pk, pub := genClientKey(t)
	fake := startFakeSSHServer(t, pub)
	bastionHost, bastionPort := splitHostPort(t, fake.addr())

	resolver := newFakeResolver()
	bastionUID := uuid.New()
	resolver.servers[bastionUID] = &store.Server{
		UID: bastionUID, Host: bastionHost, Port: bastionPort,
		Username: "tester", Protocol: store.ProtocolSSH,
		ProtocolData: &store.ServerProtocolData{SSH: &store.SSHServerData{
			PrivateKey:   pk,
			KnownHostKey: "ssh-ed25519 AAAAWRONGKEY tampered", // pinned to a bogus key
		}},
	}

	target := &store.Server{
		Host: targetHost, Port: targetPort, Protocol: store.ProtocolPostgreSQL,
		ViaUID: &bastionUID,
	}

	d := NewDialer()
	_, err := d.DialUpstream(context.Background(), resolver, testKey(), target)
	if err == nil {
		t.Fatal("DialUpstream() succeeded despite host-key mismatch")
	}
}

func assertEcho(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	msg := []byte("ping-through-tunnel")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("echo = %q, want %q", buf, msg)
	}
}

func testKey() []byte {
	return []byte("0123456789012345678901234567890X") // 32 bytes; unused for key-auth paths
}
