package shared

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
)

// The stream mechanics of a port-forward (the SPDY upgrade, the data/error
// stream pair, the net.Conn adapter) are covered against an in-process fake API
// server in internal/proxy/upstream. What these tests pin is the *dispatch*
// this package owns: which via rows are recognized, how the tunnel is built
// from a row, how it is pooled and dropped, and which nestings are refused.

// clusterRow builds a `protocol: kubernetes` server row with its ServiceAccount
// token encrypted the way the store stores it.
func clusterRow(t *testing.T, host string, port int) *store.Server {
	t.Helper()

	uid := uuid.New()

	token, err := crypto.Encrypt([]byte("sa-token"), testKey(), crypto.ServerAAD(uid.String()))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}

	return &store.Server{
		UID:               uid,
		Host:              host,
		Port:              port,
		Protocol:          store.ProtocolKubernetes,
		PasswordEncrypted: token,
		ProtocolData: &store.ServerProtocolData{
			Kubernetes: &store.KubernetesServerData{Namespace: "data", InsecureSkipTLSVerify: true},
		},
	}
}

func TestKubernetesTunnelForBuildsPoolsAndDrops(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	cluster := clusterRow(t, "api.example.invalid", 6443)
	resolver.servers[cluster.UID] = cluster

	d := NewDialer()

	tunnel, err := d.ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID)
	if err != nil {
		t.Fatalf("ConnectKubernetes() error = %v", err)
	}
	if tunnel.Namespace() != "data" {
		t.Errorf("Namespace() = %q, want %q", tunnel.Namespace(), "data")
	}

	// A second request must reuse the pooled tunnel rather than rebuild it.
	again, err := d.ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID)
	if err != nil {
		t.Fatalf("ConnectKubernetes() second error = %v", err)
	}
	if again != tunnel {
		t.Error("the tunnel was rebuilt instead of reused from the pool")
	}

	d.mu.Lock()
	pooled := len(d.tunnels)
	d.mu.Unlock()
	if pooled != 1 {
		t.Errorf("tunnel pool size = %d, want 1", pooled)
	}

	d.drop(cluster.UID)

	d.mu.Lock()
	pooled = len(d.tunnels)
	d.mu.Unlock()
	if pooled != 0 {
		t.Errorf("tunnel pool size after drop = %d, want 0", pooled)
	}

	// Close must clear the tunnel pool too, or a connectivity check would leak
	// a stale client into the next probe.
	if _, err := d.ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID); err != nil {
		t.Fatalf("ConnectKubernetes() after drop error = %v", err)
	}

	d.Close()

	d.mu.Lock()
	pooled = len(d.tunnels)
	d.mu.Unlock()
	if pooled != 0 {
		t.Errorf("tunnel pool size after Close = %d, want 0", pooled)
	}
}

// TestKubernetesTunnelForFailsClosedWithoutTrust checks the dialer's own
// refusal, independent of the API's validation: a row with neither a CA bundle
// nor the explicit opt-out gets a TOFU capture, and when that capture cannot
// happen there is no tunnel at all — never one dialed against the host's system
// trust store.
func TestKubernetesTunnelForFailsClosedWithoutTrust(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	cluster := clusterRow(t, "127.0.0.1", 1) // nothing listens on port 1
	cluster.ProtocolData.Kubernetes.InsecureSkipTLSVerify = false
	resolver.servers[cluster.UID] = cluster

	tunnel, err := NewDialer().ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID)
	if err == nil {
		t.Fatal("ConnectKubernetes(no CA, unreachable API server) succeeded")
	}

	if tunnel != nil {
		t.Error("a tunnel was returned despite no CA being pinned")
	}

	if got := resolver.k8sCACerts[cluster.UID]; got != "" {
		t.Errorf("a CA was pinned (%q) although the capture failed", got)
	}
}

// TestKubernetesTunnelPinsTheCAOnFirstConnect is the TOFU flow end to end at
// the layer that owns persistence: a row that supplied no bundle learns the API
// server's certificate, records it as *learned* (never as the supplied
// ca_cert), and the second connect reuses it instead of learning again.
func TestKubernetesTunnelPinsTheCAOnFirstConnect(t *testing.T) {
	t.Parallel()

	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	host, port := splitHostPort(t, strings.TrimPrefix(api.URL, "https://"))
	wantPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))

	resolver := newFakeResolver()
	cluster := clusterRow(t, host, port)
	cluster.ProtocolData.Kubernetes.InsecureSkipTLSVerify = false
	resolver.servers[cluster.UID] = cluster

	if _, err := NewDialer().ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID); err != nil {
		t.Fatalf("ConnectKubernetes() error = %v", err)
	}

	if got := resolver.k8sCACerts[cluster.UID]; got != wantPEM {
		t.Errorf("pinned bundle = %q, want the API server's certificate %q", got, wantPEM)
	}

	kd := resolver.servers[cluster.UID].ProtocolData.Kubernetes
	if kd.LearnedCACert != wantPEM {
		t.Errorf("learned_ca_cert = %q, want %q", kd.LearnedCACert, wantPEM)
	}

	if kd.CACert != "" {
		t.Errorf("ca_cert = %q, want the operator-supplied field left untouched", kd.CACert)
	}

	// A fresh dialer bypasses the tunnel pool, so this really is a second
	// build: with a pin on the row it must not learn anything new.
	delete(resolver.k8sCACerts, cluster.UID)

	if _, err := NewDialer().ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID); err != nil {
		t.Fatalf("ConnectKubernetes() second error = %v", err)
	}

	if got := resolver.k8sCACerts[cluster.UID]; got != "" {
		t.Errorf("the CA was re-learned (%q) although the row already carried a pin", got)
	}
}

// TestKubernetesTunnelSuppliedCAIsNeverReplacedByTOFU is binding semantic #2:
// with a bundle pasted by an operator, no capture happens at all — not even a
// harmless one that would record a second, competing value.
func TestKubernetesTunnelSuppliedCAIsNeverReplacedByTOFU(t *testing.T) {
	t.Parallel()

	supplier := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer supplier.Close()

	resolver := newFakeResolver()
	cluster := clusterRow(t, "api.example.invalid", 6443)
	cluster.ProtocolData.Kubernetes.InsecureSkipTLSVerify = false
	cluster.ProtocolData.Kubernetes.CACert = string(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: supplier.Certificate().Raw}))
	resolver.servers[cluster.UID] = cluster

	// api.example.invalid never resolves, so a capture attempt would fail loudly.
	if _, err := NewDialer().ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID); err != nil {
		t.Fatalf("ConnectKubernetes(supplied CA) error = %v", err)
	}

	if got := resolver.k8sCACerts[cluster.UID]; got != "" {
		t.Errorf("a CA was learned (%q) although the row supplied one", got)
	}
}

// unpinnableResolver is a resolver whose CA pin never sticks.
type unpinnableResolver struct{ *fakeResolver }

func (unpinnableResolver) SetKubernetesCACert(context.Context, uuid.UUID, string) error {
	return errPinRefused
}

var errPinRefused = errors.New("store is read-only")

// TestKubernetesTunnelRefusesAnUnrecordablePin: a pin that cannot be written is
// no pin at all — the next connect would learn afresh from whatever is
// presented then. Refusing is louder and safer than pretending.
func TestKubernetesTunnelRefusesAnUnrecordablePin(t *testing.T) {
	t.Parallel()

	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	host, port := splitHostPort(t, strings.TrimPrefix(api.URL, "https://"))

	resolver := newFakeResolver()
	cluster := clusterRow(t, host, port)
	cluster.ProtocolData.Kubernetes.InsecureSkipTLSVerify = false
	resolver.servers[cluster.UID] = cluster

	_, err := NewDialer().ConnectKubernetes(
		context.Background(), unpinnableResolver{resolver}, testKey(), cluster.UID)
	if !errors.Is(err, errPinRefused) {
		t.Fatalf("ConnectKubernetes() error = %v, want the pin-persistence failure", err)
	}
}

// TestDialUpstreamViaKubernetesDoesNotRetryPermanentFailures pins the retry's
// scope: it exists to recover from a moved pod, not to double the API calls
// (and evict a healthy pooled tunnel) every time a host field has a typo.
func TestDialUpstreamViaKubernetesDoesNotRetryPermanentFailures(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	cluster := clusterRow(t, "api.example.invalid", 6443)
	resolver.servers[cluster.UID] = cluster

	d := NewDialer()
	if _, err := d.ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID); err != nil {
		t.Fatalf("ConnectKubernetes() error = %v", err)
	}

	// An empty host resolves to ErrKubernetesTargetNotFound without any API
	// call, so this isolates the retry decision from the network.
	target := &store.Server{
		Host: "", Port: 5432, Protocol: store.ProtocolPostgreSQL, ViaUID: &cluster.UID,
	}

	_, err := d.DialUpstream(context.Background(), resolver, testKey(), target)
	if !errors.Is(err, upstream.ErrKubernetesTargetNotFound) {
		t.Fatalf("DialUpstream() error = %v, want ErrKubernetesTargetNotFound", err)
	}

	d.mu.Lock()
	pooled := len(d.tunnels)
	d.mu.Unlock()

	if pooled != 1 {
		t.Errorf("tunnel pool size = %d, want 1 — a permanent failure must not evict the tunnel", pooled)
	}
}

func TestKubernetesTunnelForRejectsAMissingToken(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	uid := uuid.New()
	resolver.servers[uid] = &store.Server{
		UID: uid, Host: "api.example.invalid", Port: 6443, Protocol: store.ProtocolKubernetes,
	}

	_, err := NewDialer().ConnectKubernetes(context.Background(), resolver, testKey(), uid)
	if !errors.Is(err, upstream.ErrKubernetesNoToken) {
		t.Fatalf("ConnectKubernetes() error = %v, want ErrKubernetesNoToken", err)
	}
}

func TestKubernetesTunnelForRejectsANonClusterRow(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	uid := uuid.New()
	resolver.servers[uid] = &store.Server{
		UID: uid, Host: "db.example.invalid", Port: 5432, Protocol: store.ProtocolPostgreSQL,
	}

	_, err := NewDialer().ConnectKubernetes(context.Background(), resolver, testKey(), uid)
	if !errors.Is(err, ErrViaNotTunnel) {
		t.Fatalf("ConnectKubernetes() error = %v, want ErrViaNotTunnel", err)
	}
}

// TestDialUpstreamViaKubernetesUnreachableCluster checks that an unreachable
// API server surfaces as a tunnel failure naming the target, rather than as a
// panic or as a bare TCP error the admin cannot place.
func TestDialUpstreamViaKubernetesUnreachableCluster(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	cluster := clusterRow(t, "127.0.0.1", 1) // nothing listens on port 1
	resolver.servers[cluster.UID] = cluster

	target := &store.Server{
		Host: "svc/postgres", Port: 5432, Protocol: store.ProtocolPostgreSQL,
		ViaUID: &cluster.UID,
	}

	_, err := NewDialer().DialUpstream(context.Background(), resolver, testKey(), target)
	if err == nil {
		t.Fatal("DialUpstream() succeeded against an unreachable API server")
	}
	if !strings.Contains(err.Error(), "kubernetes tunnel") {
		t.Errorf("DialUpstream() error = %v, want it to name the kubernetes tunnel", err)
	}
}

// TestSSHBastionBehindKubernetesIsRefused pins the one nesting that is
// deliberately not implemented. The store lets the chain validate, so the
// dialer must say precisely why it cannot walk it.
func TestSSHBastionBehindKubernetesIsRefused(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	cluster := clusterRow(t, "api.example.invalid", 6443)
	resolver.servers[cluster.UID] = cluster

	pk, _ := genClientKey(t)
	bastionUID := uuid.New()
	resolver.servers[bastionUID] = &store.Server{
		UID: bastionUID, Host: "bastion.example.invalid", Port: 22,
		Username: "tester", Protocol: store.ProtocolSSH,
		ProtocolData: &store.ServerProtocolData{SSH: &store.SSHServerData{PrivateKey: pk}},
		ViaUID:       &cluster.UID,
	}

	target := &store.Server{
		Host: "db.example.invalid", Port: 5432, Protocol: store.ProtocolPostgreSQL,
		ViaUID: &bastionUID,
	}

	_, err := NewDialer().DialUpstream(context.Background(), resolver, testKey(), target)
	if !errors.Is(err, ErrKubernetesViaUnsupported) {
		t.Fatalf("DialUpstream() error = %v, want ErrKubernetesViaUnsupported", err)
	}
}

// TestKubernetesClusterBehindAnSSHBastion covers the supported nesting: the API
// server is only reachable through a jump host, so building the tunnel has to
// establish the bastion connection first and hand its dial function down.
func TestKubernetesClusterBehindAnSSHBastion(t *testing.T) {
	t.Parallel()

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

	cluster := clusterRow(t, "api.example.invalid", 6443)
	cluster.ViaUID = &bastionUID
	resolver.servers[cluster.UID] = cluster

	d := NewDialer()

	if _, err := d.ConnectKubernetes(context.Background(), resolver, testKey(), cluster.UID); err != nil {
		t.Fatalf("ConnectKubernetes(via bastion) error = %v", err)
	}

	d.mu.Lock()
	clients, tunnels := len(d.clients), len(d.tunnels)
	d.mu.Unlock()

	if clients != 1 {
		t.Errorf("ssh pool size = %d, want 1 (the bastion must have been dialed)", clients)
	}
	if tunnels != 1 {
		t.Errorf("tunnel pool size = %d, want 1", tunnels)
	}
}
