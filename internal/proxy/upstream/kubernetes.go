package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	apispdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	clientspdy "k8s.io/client-go/transport/spdy"

	authv1 "k8s.io/api/authorization/v1"
)

// This file is the *only* place in dbbat that imports k8s.io/client-go. Every
// other layer (the shared dialer, conncheck, the API) talks to a
// *KubernetesTunnel and matches the sentinel errors below, so the client-go
// dependency tree — which roughly triples go.sum — stays contained here.

// Sentinel errors returned by a KubernetesTunnel. They exist so callers can
// classify a failure without importing k8s.io/apimachinery's error helpers.
var (
	// ErrKubernetesNoToken means the cluster row carries no ServiceAccount
	// bearer token. There is no other supported credential: EKS/GKE kubeconfigs
	// authenticate through exec credential plugins, which a server daemon
	// cannot run, so dbbat deliberately does not accept kubeconfig uploads.
	ErrKubernetesNoToken = errors.New("kubernetes: the cluster row has no service account token")
	// ErrKubernetesUnauthorized means the API server rejected the token (401):
	// it expired, was revoked, or belongs to a different cluster.
	ErrKubernetesUnauthorized = errors.New("kubernetes: the API server rejected the service account token")
	// ErrKubernetesForbidden means the token authenticated but RBAC denied the
	// action (403) — almost always a missing `pods/portforward` verb.
	ErrKubernetesForbidden = errors.New("kubernetes: the service account is not authorized for this action")
	// ErrKubernetesTargetNotFound means the addressed pod or service does not
	// exist in the tunnel's namespace.
	ErrKubernetesTargetNotFound = errors.New("kubernetes: target pod or service not found in the namespace")
	// ErrKubernetesTargetNotReady means the service exists but no ready
	// endpoint backs it, so there is nothing to port-forward to.
	ErrKubernetesTargetNotReady = errors.New("kubernetes: no ready pod backs the target")
)

// portForwardTimeout bounds the stream upgrade, matching the shared dialer's
// own per-dial budget.
const portForwardTimeout = 30 * time.Second

// KubernetesConfig is everything a KubernetesTunnel needs, expressed in terms
// the store already holds: the API server address, the ServiceAccount bearer
// token (which lives in the row's encrypted password column), the public CA
// bundle and the namespace.
type KubernetesConfig struct {
	// Host is the API server host, with or without a scheme; Port completes it.
	Host string
	Port int
	// Token is the decrypted ServiceAccount bearer token.
	Token string
	// CACert is the API server's PEM CA bundle. Empty falls back to the host's
	// system trust store — which is right for a cluster behind a publicly
	// trusted certificate and wrong for the usual self-signed one.
	CACert string
	// Namespace scopes every lookup and every port-forward.
	Namespace string
	// InsecureSkipTLSVerify disables API server certificate verification.
	InsecureSkipTLSVerify bool
	// DialContext, when set, supplies every TCP connection to the API server —
	// this is how a cluster row hangs off an SSH bastion. Setting it forces the
	// SPDY transport: see newStreamDialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// UserAgent is sent on every API request; empty defaults to "dbbat".
	UserAgent string
}

// KubernetesTunnel opens `pods/portforward` streams to pods in one namespace of
// one cluster, and answers the questions a connectivity check asks along the
// way (is the token valid, may we port-forward, does the target resolve).
//
// It is safe for concurrent use and holds no per-connection state: one tunnel
// is pooled per cluster row and every database session opens (and owns) its own
// upgraded connection through it. Unlike the SSH pool there is no shared
// transport to keep alive — a port-forward upgrade is bound to one pod, so
// what is worth pooling is the client, its TLS material and the token, not a
// socket.
type KubernetesTunnel struct {
	restConfig *restclient.Config
	clientset  kubernetes.Interface
	namespace  string
}

// NewKubernetesTunnel builds a tunnel from a cluster row's material. It does no
// I/O: an unreachable API server surfaces on the first call, not here.
func NewKubernetesTunnel(cfg KubernetesConfig) (*KubernetesTunnel, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, ErrKubernetesNoToken
	}

	host, err := cfg.apiServerURL()
	if err != nil {
		return nil, err
	}

	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = "dbbat"
	}

	rc := &restclient.Config{
		Host:        host,
		BearerToken: cfg.Token,
		UserAgent:   userAgent,
		Timeout:     portForwardTimeout,
		Dial:        cfg.DialContext,
	}

	// A CA bundle and Insecure are mutually exclusive as far as rest.Config
	// validation is concerned, and "verify against this CA" is always the
	// stronger statement — so it wins when an operator set both.
	switch {
	case cfg.CACert != "":
		rc.TLSClientConfig.CAData = []byte(cfg.CACert)
	case cfg.InsecureSkipTLSVerify:
		rc.TLSClientConfig.Insecure = true
	}

	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: failed to build an API client: %w", err)
	}

	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		namespace = "default"
	}

	return &KubernetesTunnel{restConfig: rc, clientset: clientset, namespace: namespace}, nil
}

// apiServerURL normalizes Host/Port into a single https URL. Operators type the
// API server in every shape kubectl accepts ("https://1.2.3.4", "1.2.3.4:6443",
// a bare hostname), and the row carries a separate port column, so the two have
// to be reconciled rather than concatenated.
func (c KubernetesConfig) apiServerURL() (string, error) {
	raw := strings.TrimSpace(c.Host)
	if raw == "" {
		return "", errors.New("kubernetes: the cluster row has no API server host")
	}

	scheme := "https"
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme = raw[:i]
		raw = raw[i+3:]
	}

	raw = strings.TrimSuffix(raw, "/")
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		raw = raw[:i]
	}

	port := c.Port
	// An embedded port in the host field only wins when the row has none.
	if h, p, err := net.SplitHostPort(raw); err == nil {
		raw = h
		if port == 0 {
			if parsed, convErr := strconv.Atoi(p); convErr == nil {
				port = parsed
			}
		}
	}

	if port == 0 {
		port = 443
	}

	return scheme + "://" + net.JoinHostPort(raw, strconv.Itoa(port)), nil
}

// Namespace is the namespace this tunnel is scoped to.
func (t *KubernetesTunnel) Namespace() string { return t.namespace }

// ResolvePod turns a target row's host field into a pod name.
//
// `svc/<name>` (or `service/<name>`) resolves through the service's
// EndpointSlices to a *ready* endpoint, exactly as `kubectl port-forward
// svc/...` does. Anything else is taken as a pod name verbatim — with an
// optional `pod/` prefix stripped — and costs no API call.
//
// Resolution runs on every dial rather than being cached with the tunnel:
// pods move, and a stale name is precisely what the dialer's retry has to
// recover from.
func (t *KubernetesTunnel) ResolvePod(ctx context.Context, host string) (string, error) {
	target := strings.TrimSpace(host)

	switch {
	case strings.HasPrefix(target, "svc/"):
		return t.resolveService(ctx, strings.TrimPrefix(target, "svc/"))
	case strings.HasPrefix(target, "service/"):
		return t.resolveService(ctx, strings.TrimPrefix(target, "service/"))
	case strings.HasPrefix(target, "pod/"):
		return strings.TrimPrefix(target, "pod/"), nil
	default:
		return target, nil
	}
}

// resolveService picks a ready pod backing a service, via its EndpointSlices.
//
// EndpointSlices rather than the legacy Endpoints object because that is what
// modern clusters actually populate, and readiness is honoured because
// forwarding to a pod the service itself would not route to is a confusing way
// to fail.
func (t *KubernetesTunnel) resolveService(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty service name", ErrKubernetesTargetNotFound)
	}

	slices, err := t.clientset.DiscoveryV1().EndpointSlices(t.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + name,
	})
	if err != nil {
		return "", t.classify(err, "service "+name)
	}

	for i := range slices.Items {
		for _, endpoint := range slices.Items[i].Endpoints {
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			if endpoint.TargetRef != nil && endpoint.TargetRef.Kind == "Pod" && endpoint.TargetRef.Name != "" {
				return endpoint.TargetRef.Name, nil
			}
		}
	}

	if len(slices.Items) == 0 {
		return "", fmt.Errorf("%w: no endpointslice for service %s/%s", ErrKubernetesTargetNotFound, t.namespace, name)
	}

	return "", fmt.Errorf("%w: service %s/%s", ErrKubernetesTargetNotReady, t.namespace, name)
}

// CheckPodReady verifies that a pod exists and is Ready. The dial path skips
// this (the kubelet will refuse soon enough); the connectivity check runs it so
// an admin learns which of "wrong name" and "pod not up" they are looking at.
func (t *KubernetesTunnel) CheckPodReady(ctx context.Context, podName string) error {
	pod, err := t.clientset.CoreV1().Pods(t.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return t.classify(err, "pod "+podName)
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return nil
		}
	}

	return fmt.Errorf("%w: pod %s/%s is %s", ErrKubernetesTargetNotReady, t.namespace, podName, pod.Status.Phase)
}

// PortForwardAllowed asks the API server, through a SelfSubjectAccessReview,
// whether this ServiceAccount may create `pods/portforward` in the namespace.
//
// It is the difference between telling an admin "add `create` on
// `pods/portforward` to the Role" and making them read a 403 out of a stream
// upgrade failure. The review itself needs no RBAC: system:basic-user grants it
// to every authenticated identity, ServiceAccounts included.
func (t *KubernetesTunnel) PortForwardAllowed(ctx context.Context) (bool, string, error) {
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace:   t.namespace,
				Verb:        "create",
				Resource:    "pods",
				Subresource: "portforward",
			},
		},
	}

	result, err := t.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, "", t.classify(err, "selfsubjectaccessreview")
	}

	return result.Status.Allowed, result.Status.Reason, nil
}

// DialPod opens one port-forward stream pair to podName's containerPort and
// returns it as a net.Conn.
func (t *KubernetesTunnel) DialPod(ctx context.Context, podName string, containerPort int) (net.Conn, error) {
	if podName == "" {
		return nil, fmt.Errorf("%w: empty pod name", ErrKubernetesTargetNotFound)
	}

	streamURL := t.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(t.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	dialer, err := t.newStreamDialer(streamURL)
	if err != nil {
		return nil, err
	}

	conn, protocol, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, t.classify(err, "port-forward to pod "+podName)
	}

	if protocol != portforward.PortForwardProtocolV1Name {
		_ = conn.Close()

		return nil, fmt.Errorf("kubernetes: the API server negotiated %q, not %q",
			protocol, portforward.PortForwardProtocolV1Name)
	}

	pfConn, err := newPortForwardConn(conn, t.namespace, podName, containerPort)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return pfConn, nil
}

// Dial resolves host (a pod name or `svc/<name>`) and opens a stream to port.
func (t *KubernetesTunnel) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	podName, err := t.ResolvePod(ctx, host)
	if err != nil {
		return nil, err
	}

	return t.DialPod(ctx, podName, port)
}

// newStreamDialer builds the httpstream dialer used for the upgrade.
//
// Deliberately the *low-level* dialer, not tools/portforward.PortForwarder:
// the high-level one binds local TCP listeners and copies between them, which
// is the wrong shape entirely — we need a net.Conn, not a localhost port.
//
// Transport choice: normally websocket-first with a SPDY fallback, matching
// kubectl (SPDY is deprecated but still all that older API servers speak).
// When the cluster is reached through an SSH bastion the websocket transport is
// skipped: client-go's websocket round tripper exposes no dial hook, so it
// would ignore rest.Config.Dial and try to reach the API server directly. The
// SPDY round tripper can be handed a substitute transport, so that is the one
// that can honour a bastion.
func (t *KubernetesTunnel) newStreamDialer(streamURL *url.URL) (httpstream.Dialer, error) {
	spdyDialer, err := t.newSPDYDialer(streamURL)
	if err != nil {
		return nil, err
	}

	if t.restConfig.Dial != nil {
		return spdyDialer, nil
	}

	wsDialer, err := portforward.NewSPDYOverWebsocketDialer(streamURL, t.restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: failed to build the websocket dialer: %w", err)
	}

	return portforward.NewFallbackDialer(wsDialer, spdyDialer, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	}), nil
}

// newSPDYDialer builds the SPDY upgrade dialer. It reimplements
// transport/spdy.RoundTripperFor rather than calling it because that helper
// wires the TLS config in directly, leaving no way to supply a dial function —
// and a substitute transport is exactly how the SPDY round tripper takes one.
func (t *KubernetesTunnel) newSPDYDialer(streamURL *url.URL) (httpstream.Dialer, error) {
	tlsConfig, err := restclient.TLSConfigFor(t.restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: invalid API server TLS configuration: %w", err)
	}

	proxy := http.ProxyFromEnvironment
	if t.restConfig.Proxy != nil {
		proxy = t.restConfig.Proxy
	}

	upgradeTransport := utilnet.SetTransportDefaults(&http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           proxy,
		DialContext:     t.restConfig.Dial,
	})

	roundTripper, err := apispdy.NewRoundTripperWithConfig(apispdy.RoundTripperConfig{
		UpgradeTransport: upgradeTransport,
		PingPeriod:       5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("kubernetes: failed to build the SPDY transport: %w", err)
	}

	wrapper, err := restclient.HTTPWrappersForConfig(t.restConfig, roundTripper)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: failed to wrap the SPDY transport: %w", err)
	}

	return clientspdy.NewDialer(roundTripper, &http.Client{Transport: wrapper}, http.MethodPost, streamURL), nil
}

// classify maps an API/stream error onto one of this package's sentinels so the
// layers above never have to import k8s.io/apimachinery to tell "the token is
// dead" from "you lack a verb".
//
// Status errors are matched structurally; the stream-upgrade path is matched on
// text as well, because a failed SPDY/websocket upgrade surfaces as a wrapped
// HTTP error rather than a metav1.Status.
func (t *KubernetesTunnel) classify(err error, what string) error {
	if err == nil {
		return nil
	}

	switch {
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w (%s): %v", ErrKubernetesUnauthorized, what, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w (%s): %v", ErrKubernetesForbidden, what, err)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w (%s): %v", ErrKubernetesTargetNotFound, what, err)
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "401 unauthorized"), strings.Contains(msg, "\"unauthorized\""):
		return fmt.Errorf("%w (%s): %v", ErrKubernetesUnauthorized, what, err)
	case strings.Contains(msg, "403 forbidden"), strings.Contains(msg, "is forbidden"):
		return fmt.Errorf("%w (%s): %v", ErrKubernetesForbidden, what, err)
	case strings.Contains(msg, "404 not found"), strings.Contains(msg, "not found"):
		return fmt.Errorf("%w (%s): %v", ErrKubernetesTargetNotFound, what, err)
	}

	return fmt.Errorf("kubernetes: %s failed: %w", what, err)
}
