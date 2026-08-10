package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKubernetesConfigAPIServerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  KubernetesConfig
		want string
	}{
		{"bare host with port column", KubernetesConfig{Host: "api.example.com", Port: 6443}, "https://api.example.com:6443"},
		{"scheme in host", KubernetesConfig{Host: "https://api.example.com", Port: 6443}, "https://api.example.com:6443"},
		{"port embedded in host, none on the row", KubernetesConfig{Host: "https://api.example.com:6443"}, "https://api.example.com:6443"},
		{"row port wins over the embedded one", KubernetesConfig{Host: "api.example.com:1", Port: 6443}, "https://api.example.com:6443"},
		{"no port anywhere defaults to 443", KubernetesConfig{Host: "api.example.com"}, "https://api.example.com:443"},
		{"trailing path is dropped", KubernetesConfig{Host: "https://api.example.com/", Port: 6443}, "https://api.example.com:6443"},
		{"bare ipv6", KubernetesConfig{Host: "::1", Port: 6443}, "https://[::1]:6443"},
		{"bracketed ipv6 with port", KubernetesConfig{Host: "[::1]:6443"}, "https://[::1]:6443"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.cfg.apiServerURL()
			if err != nil {
				t.Fatalf("apiServerURL() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("apiServerURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewKubernetesTunnelRequiresAToken(t *testing.T) {
	t.Parallel()

	_, err := NewKubernetesTunnel(KubernetesConfig{Host: "api.example.com", Port: 6443})
	if !errors.Is(err, ErrKubernetesNoToken) {
		t.Fatalf("NewKubernetesTunnel(no token) error = %v, want ErrKubernetesNoToken", err)
	}
}

func TestNewKubernetesTunnelRequiresAHost(t *testing.T) {
	t.Parallel()

	_, err := NewKubernetesTunnel(KubernetesConfig{Token: "t"})
	if err == nil || !strings.Contains(err.Error(), "API server host") {
		t.Fatalf("NewKubernetesTunnel(no host) error = %v, want a missing-host error", err)
	}
}

func TestNewKubernetesTunnelDefaultsTheNamespace(t *testing.T) {
	t.Parallel()

	tunnel, err := NewKubernetesTunnel(KubernetesConfig{Host: "api.example.com", Port: 6443, Token: "t"})
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}
	if tunnel.Namespace() != "default" {
		t.Errorf("Namespace() = %q, want %q", tunnel.Namespace(), "default")
	}
}

// TestKubernetesTunnelDialPodRoundTrips is the load-bearing test: a real SPDY
// port-forward upgrade against an in-process endpoint, with bytes going through
// the net.Conn adapter in both directions.
func TestKubernetesTunnelDialPodRoundTrips(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	tunnel := fake.tunnel(t)

	conn, err := tunnel.DialPod(context.Background(), "pg-0", 5432)
	if err != nil {
		t.Fatalf("DialPod() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("hello pod")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	buf := make([]byte, 9)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	if string(buf) != "hello pod" {
		t.Errorf("echoed %q, want %q", buf, "hello pod")
	}

	if ports := fake.seenPorts(); len(ports) != 1 || ports[0] != "5432" {
		t.Errorf("port header = %v, want [5432]", ports)
	}

	// The bearer token must reach the API server: it is the only credential.
	var sawToken bool
	for _, h := range fake.seenAuth() {
		if h == "Bearer sa-token" {
			sawToken = true
		}
	}
	if !sawToken {
		t.Errorf("no request carried the service account token; saw %v", fake.seenAuth())
	}
}

func TestKubernetesTunnelConnAddressesAndDeadlines(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	tunnel := fake.tunnel(t)

	conn, err := tunnel.DialPod(context.Background(), "pg-0", 5432)
	if err != nil {
		t.Fatalf("DialPod() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if got := conn.RemoteAddr().String(); got != "data/pg-0:5432" {
		t.Errorf("RemoteAddr() = %q, want %q", got, "data/pg-0:5432")
	}
	if got := conn.RemoteAddr().Network(); got != "kubernetes-portforward" {
		t.Errorf("RemoteAddr().Network() = %q, want %q", got, "kubernetes-portforward")
	}
	if got := conn.LocalAddr().String(); got != "data/dbbat" {
		t.Errorf("LocalAddr() = %q, want %q", got, "data/dbbat")
	}

	// Deadlines are unsupported, exactly like the SSH channel conns.
	for name, err := range map[string]error{
		"SetDeadline":      conn.SetDeadline(time.Now()),
		"SetReadDeadline":  conn.SetReadDeadline(time.Now()),
		"SetWriteDeadline": conn.SetWriteDeadline(time.Now()),
	} {
		if !errors.Is(err, os.ErrNoDeadline) {
			t.Errorf("%s() = %v, want os.ErrNoDeadline", name, err)
		}
	}
}

func TestKubernetesTunnelConnCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	tunnel := fake.tunnel(t)

	conn, err := tunnel.DialPod(context.Background(), "pg-0", 5432)
	if err != nil {
		t.Fatalf("DialPod() error = %v", err)
	}

	first := conn.Close()
	if second := conn.Close(); !errors.Is(second, first) {
		t.Errorf("second Close() = %v, want the first result %v", second, first)
	}
}

// TestKubernetesTunnelSurfacesTheForwardError covers the failure that is
// invisible without the error stream: the upgrade succeeds, so the only signal
// that nothing listens on the container port is what the kubelet writes there.
func TestKubernetesTunnelSurfacesTheForwardError(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.forwardError = "unable to do port forwarding: socat not found"

	tunnel := fake.tunnel(t)

	conn, err := tunnel.DialPod(context.Background(), "pg-0", 9999)
	if err != nil {
		t.Fatalf("DialPod() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, readErr := io.ReadAll(conn)
	if readErr == nil {
		t.Fatal("Read() succeeded, want the kubelet's forwarding error")
	}
	if !strings.Contains(readErr.Error(), "socat not found") {
		t.Errorf("Read() error = %v, want it to carry the error stream message", readErr)
	}
}

func TestKubernetesTunnelClassifiesUpgradeFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"missing rbac", 403, ErrKubernetesForbidden},
		{"dead token", 401, ErrKubernetesUnauthorized},
		{"no such pod", 404, ErrKubernetesTargetNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeCluster(t)
			fake.portForwardStatus = tc.status

			_, err := fake.tunnel(t).DialPod(context.Background(), "pg-0", 5432)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DialPod() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestKubernetesTunnelResolvePod(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.slices = readyEndpointSlices("pg-not-ready", "pg-ready")

	tunnel := fake.tunnel(t)

	tests := []struct {
		name string
		host string
		want string
	}{
		{"bare pod name is verbatim", "pg-0", "pg-0"},
		{"pod/ prefix is stripped", "pod/pg-0", "pg-0"},
		{"svc/ resolves to the ready endpoint", "svc/postgres", "pg-ready"},
		{"service/ is the long form", "service/postgres", "pg-ready"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tunnel.ResolvePod(context.Background(), tc.host)
			if err != nil {
				t.Fatalf("ResolvePod(%q) error = %v", tc.host, err)
			}
			if got != tc.want {
				t.Errorf("ResolvePod(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

// TestKubernetesTunnelResolvePodSkipsAPICallsForPodNames pins the property the
// retry path depends on: naming a pod directly costs nothing, so re-dialing is
// cheap, while `svc/` deliberately re-queries.
func TestKubernetesTunnelResolvePodSkipsAPICallsForPodNames(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	tunnel := fake.tunnel(t)

	if _, err := tunnel.ResolvePod(context.Background(), "pg-0"); err != nil {
		t.Fatalf("ResolvePod() error = %v", err)
	}

	fake.mu.Lock()
	paths := len(fake.requestPaths)
	fake.mu.Unlock()

	if paths != 0 {
		t.Errorf("ResolvePod(pod name) made %d API requests, want 0", paths)
	}
}

func TestKubernetesTunnelResolveServiceFailures(t *testing.T) {
	t.Parallel()

	t.Run("no endpointslice at all", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)

		_, err := fake.tunnel(t).ResolvePod(context.Background(), "svc/postgres")
		if !errors.Is(err, ErrKubernetesTargetNotFound) {
			t.Fatalf("ResolvePod() error = %v, want ErrKubernetesTargetNotFound", err)
		}
	})

	t.Run("endpointslice with no ready endpoint", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)
		notReady := false
		slices := readyEndpointSlices("a", "b")
		slices.Items[0].Endpoints[1].Conditions.Ready = &notReady
		fake.slices = slices

		_, err := fake.tunnel(t).ResolvePod(context.Background(), "svc/postgres")
		if !errors.Is(err, ErrKubernetesTargetNotReady) {
			t.Fatalf("ResolvePod() error = %v, want ErrKubernetesTargetNotReady", err)
		}
	})

	t.Run("empty service name", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)

		_, err := fake.tunnel(t).ResolvePod(context.Background(), "svc/")
		if !errors.Is(err, ErrKubernetesTargetNotFound) {
			t.Fatalf("ResolvePod() error = %v, want ErrKubernetesTargetNotFound", err)
		}
	})
}

func TestKubernetesTunnelDialResolvesThenForwards(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.slices = readyEndpointSlices("pg-not-ready", "pg-ready")

	conn, err := fake.tunnel(t).Dial(context.Background(), "svc/postgres", 5432)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if got := conn.RemoteAddr().String(); got != "data/pg-ready:5432" {
		t.Errorf("RemoteAddr() = %q, want the ready pod", got)
	}
}

func TestKubernetesTunnelCheckPodReady(t *testing.T) {
	t.Parallel()

	t.Run("ready", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)
		fake.pod = podWithReady(corev1.ConditionTrue, corev1.PodRunning)

		if err := fake.tunnel(t).CheckPodReady(context.Background(), "pg-0"); err != nil {
			t.Fatalf("CheckPodReady() error = %v", err)
		}
	})

	t.Run("exists but not ready", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)
		fake.pod = podWithReady(corev1.ConditionFalse, corev1.PodPending)

		err := fake.tunnel(t).CheckPodReady(context.Background(), "pg-0")
		if !errors.Is(err, ErrKubernetesTargetNotReady) {
			t.Fatalf("CheckPodReady() error = %v, want ErrKubernetesTargetNotReady", err)
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)

		err := fake.tunnel(t).CheckPodReady(context.Background(), "pg-0")
		if !errors.Is(err, ErrKubernetesTargetNotFound) {
			t.Fatalf("CheckPodReady() error = %v, want ErrKubernetesTargetNotFound", err)
		}
	})
}

func TestKubernetesTunnelPortForwardAllowed(t *testing.T) {
	t.Parallel()

	t.Run("allowed", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)
		fake.allowed = true

		allowed, _, err := fake.tunnel(t).PortForwardAllowed(context.Background())
		if err != nil {
			t.Fatalf("PortForwardAllowed() error = %v", err)
		}
		if !allowed {
			t.Error("PortForwardAllowed() = false, want true")
		}
	})

	t.Run("denied carries the reason", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCluster(t)
		fake.reason = "no RBAC policy matched"

		allowed, reason, err := fake.tunnel(t).PortForwardAllowed(context.Background())
		if err != nil {
			t.Fatalf("PortForwardAllowed() error = %v", err)
		}
		if allowed {
			t.Error("PortForwardAllowed() = true, want false")
		}
		if reason != "no RBAC policy matched" {
			t.Errorf("reason = %q, want the API server's reason", reason)
		}
	})
}

// TestKubernetesTunnelHonoursTheBastionDial covers the "cluster reached through
// an SSH bastion" wiring: every TCP connection to the API server must go
// through the supplied dial function, including the port-forward upgrade — the
// case that forced the hand-built SPDY transport.
func TestKubernetesTunnelHonoursTheBastionDial(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)

	var dials atomic.Int64

	cfg := fake.config()
	cfg.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)

		var nd net.Dialer

		return nd.DialContext(ctx, network, addr)
	}

	tunnel, err := NewKubernetesTunnel(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	conn, err := tunnel.DialPod(context.Background(), "pg-0", 5432)
	if err != nil {
		t.Fatalf("DialPod() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}

	if dials.Load() == 0 {
		t.Error("the port-forward upgrade bypassed the supplied dial function")
	}
}

func TestKubernetesTunnelSendsAUserAgent(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)
	fake.allowed = true

	cfg := fake.config()
	cfg.UserAgent = "dbbat/port-forward"

	tunnel, err := NewKubernetesTunnel(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	if _, _, err := tunnel.PortForwardAllowed(context.Background()); err != nil {
		t.Fatalf("PortForwardAllowed() error = %v", err)
	}

	for _, ua := range fake.seenUserAgents() {
		if strings.Contains(ua, "dbbat/port-forward") {
			return
		}
	}

	t.Errorf("no request identified dbbat in the User-Agent; saw %v", fake.seenUserAgents())
}

func TestKubernetesTunnelRejectsAnEmptyPodName(t *testing.T) {
	t.Parallel()

	fake := newFakeCluster(t)

	_, err := fake.tunnel(t).DialPod(context.Background(), "", 5432)
	if !errors.Is(err, ErrKubernetesTargetNotFound) {
		t.Fatalf("DialPod(\"\") error = %v, want ErrKubernetesTargetNotFound", err)
	}
}

func podWithReady(status corev1.ConditionStatus, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-0", Namespace: "data"},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}
