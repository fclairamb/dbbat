package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authorization/v1"
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
	// ErrKubernetesNoHost means the cluster row carries no API server address.
	ErrKubernetesNoHost = errors.New("kubernetes: the cluster row has no API server host")
	// ErrKubernetesNoCACert means the row pins no CA bundle — neither supplied
	// nor learned — and has not explicitly opted out of verification. Refusing
	// is the fail-closed choice: without it, client-go falls back to the
	// *host's* system trust store, which silently accepts any publicly trusted
	// certificate for that hostname — a far weaker guarantee than the operator
	// asked for, applied to the connection that carries the ServiceAccount
	// token. TOFU adds a *first* connect that learns (LearnKubernetesCACert);
	// it never adds a connect that falls back to the system store.
	ErrKubernetesNoCACert = errors.New(
		"kubernetes: the cluster row pins no CA bundle and has not set insecure_skip_tls_verify")
	// ErrKubernetesCAPinMismatch means the API server presented a certificate
	// that the *TOFU-learned* bundle does not vouch for. It is reported apart
	// from a plain "certificate not trusted" on purpose: with a learned pin
	// there was a connect that worked, so this is either the cluster CA
	// rotating or somebody sitting in the middle — and an operator has to be
	// able to tell those two apart before deciding to re-pin.
	ErrKubernetesCAPinMismatch = errors.New(
		"kubernetes: the API server's certificate does not match the CA pinned on first connect")
	// ErrKubernetesCANotPinnable means the TLS handshake completed but the API
	// server presented no certificate to learn from.
	ErrKubernetesCANotPinnable = errors.New("kubernetes: the API server presented no certificate to pin")
	// ErrKubernetesProtocolMismatch means the API server negotiated something
	// other than the port-forward subprotocol.
	ErrKubernetesProtocolMismatch = errors.New("kubernetes: the API server negotiated an unexpected subprotocol")
)

// IsPermanentError reports whether err will fail identically however many
// times it is retried: a misconfigured row, a dead token, a missing RBAC verb,
// a pod that is simply not there.
//
// It exists so the shared dialer's drop-and-retry — which is there to recover
// from a moved pod — does not double every API call and evict the pooled
// tunnel on a plain typo in the host field.
func IsPermanentError(err error) bool {
	for _, sentinel := range []error{
		ErrKubernetesNoToken,
		ErrKubernetesNoHost,
		ErrKubernetesNoCACert,
		ErrKubernetesCAPinMismatch,
		ErrKubernetesUnauthorized,
		ErrKubernetesForbidden,
		ErrKubernetesTargetNotFound,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	// ErrKubernetesTargetNotReady is deliberately absent: a pod that is not
	// Ready *now* is the one case where re-resolving may legitimately land on
	// a different, healthy endpoint.
	return false
}

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
	// CACert is the API server's PEM CA bundle as the operator supplied it. It
	// always wins: when it is set, verification runs against it and no TOFU
	// pin is ever consulted or learned.
	CACert string
	// LearnedCACert is the bundle a previous connect pinned itself, used only
	// when CACert is empty. A tunnel built on it reports a verification failure
	// as ErrKubernetesCAPinMismatch rather than as an untrusted certificate.
	//
	// One of CACert, LearnedCACert or InsecureSkipTLSVerify must be set: an
	// empty trust configuration is rejected rather than quietly falling back to
	// the host's system trust store. LearnKubernetesCACert is what fills this
	// in, and it is a deliberate call, never a fallback.
	LearnedCACert string
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
	// learnedPin records that this tunnel verifies against a TOFU-learned
	// bundle rather than a supplied one, which is what lets classify report a
	// trust failure as a *changed* CA instead of an unconfigured one.
	learnedPin bool
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
	// stronger statement — so it wins when an operator set both. The order
	// below is the trust hierarchy: what the operator pasted, then what the
	// operator explicitly opted out of, then what we learned ourselves.
	//
	// None of them is a hard error rather than a silent fallback: leaving them
	// all unset yields a tls.Config with a nil RootCAs, i.e. the host's system
	// trust store, which is not what "no CA configured" should ever mean here.
	// The API no longer *requires* a pasted bundle — a row with none gets a
	// TOFU pin instead — but "learn one" is an explicit call the caller makes
	// (LearnKubernetesCACert), never something this constructor does behind
	// its back, and never the system store.
	var learnedPin bool

	switch {
	case cfg.CACert != "":
		rc.CAData = []byte(cfg.CACert)
	case cfg.InsecureSkipTLSVerify:
		rc.Insecure = true
	case cfg.LearnedCACert != "":
		rc.CAData = []byte(cfg.LearnedCACert)
		learnedPin = true
	default:
		return nil, ErrKubernetesNoCACert
	}

	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: failed to build an API client: %w", err)
	}

	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		namespace = "default"
	}

	return &KubernetesTunnel{restConfig: rc, clientset: clientset, namespace: namespace, learnedPin: learnedPin}, nil
}

// LearnKubernetesCACert performs the trust-on-first-use capture: it opens one
// TLS connection to the API server, records the certificate chain it presents,
// and returns it PEM-encoded so the caller can persist it as the row's learned
// bundle.
//
// It is a *dedicated* handshake rather than a hook inside the API client for
// two reasons. rest.Config exposes no VerifyPeerCertificate callback, so
// bending the client into capturing a chain means replacing its transport
// wholesale — on both the REST path and the two stream-upgrade paths, each of
// which would silently fall back to the system trust store if any of them were
// missed. And this way nothing is ever sent to an unverified server: this
// connection carries no request, so the ServiceAccount token does not travel
// until a bundle is pinned.
//
// What gets pinned is the chain's *CA* certificates when the server sends any,
// which makes this an issuer pin — weaker than SSH's exact-key pin, and the
// reason a supplied bundle stays the recommendation (see docs/kubernetes.md).
func LearnKubernetesCACert(ctx context.Context, cfg KubernetesConfig) (string, error) {
	rawURL, err := cfg.apiServerURL()
	if err != nil {
		return "", err
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("kubernetes: invalid API server address %q: %w", rawURL, err)
	}

	dial := cfg.DialContext
	if dial == nil {
		nd := &net.Dialer{Timeout: portForwardTimeout}
		dial = nd.DialContext
	}

	raw, err := dial(ctx, "tcp", parsed.Host)
	if err != nil {
		return "", fmt.Errorf("kubernetes: could not reach the API server at %s: %w", parsed.Host, err)
	}

	defer func() { _ = raw.Close() }()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(portForwardTimeout)
	}

	_ = raw.SetDeadline(deadline)

	var presented [][]byte

	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: parsed.Hostname(),
		// Nothing to verify against yet — that is what "first use" means. The
		// chain is captured below and becomes the thing every later connect is
		// verified against.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			presented = rawCerts

			return nil
		},
	})

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return "", fmt.Errorf("kubernetes: TLS handshake with the API server at %s failed: %w", parsed.Host, err)
	}

	_ = tlsConn.Close()

	return pinnableChainPEM(presented)
}

// pinnableChainPEM turns a presented certificate chain into the bundle to pin.
//
// CA certificates are preferred, so the pin follows the *issuer* and survives
// the API server's serving certificate being reissued — the routine event.
// A server that presents only its leaf leaves nothing issuer-shaped, so the
// leaf itself is pinned: stricter, but it then breaks on serving-certificate
// rotation rather than on CA rotation.
func pinnableChainPEM(rawCerts [][]byte) (string, error) {
	if len(rawCerts) == 0 {
		return "", ErrKubernetesCANotPinnable
	}

	var cas, all []*x509.Certificate

	for _, der := range rawCerts {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return "", fmt.Errorf("kubernetes: the API server presented an unparseable certificate: %w", err)
		}

		all = append(all, cert)

		if cert.IsCA && cert.BasicConstraintsValid {
			cas = append(cas, cert)
		}
	}

	chosen := cas
	if len(chosen) == 0 {
		chosen = all
	}

	var buf bytes.Buffer

	for _, cert := range chosen {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			return "", fmt.Errorf("kubernetes: failed to encode the learned CA bundle: %w", err)
		}
	}

	return buf.String(), nil
}

// apiServerURL normalizes Host/Port into a single https URL. Operators type the
// API server in every shape kubectl accepts ("https://1.2.3.4", "1.2.3.4:6443",
// a bare hostname), and the row carries a separate port column, so the two have
// to be reconciled rather than concatenated.
func (c KubernetesConfig) apiServerURL() (string, error) {
	raw := strings.TrimSpace(c.Host)
	if raw == "" {
		return "", ErrKubernetesNoHost
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
// modern clusters actually populate, and readiness is honored because
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

// PortForwardAccess reports which upgrade transports a ServiceAccount is
// admitted to use for `pods/portforward`, one bool per RBAC verb the API
// server derives from that transport's HTTP method: the websocket upgrade is
// a GET (`get`), the SPDY upgrade a POST (`create`). "May we port-forward
// here?" has two answers because the tunnel has two transports, and a Role
// need not grant both.
type PortForwardAccess struct {
	// Create reports whether `create` on `pods/portforward` is granted, which
	// is what admits the SPDY transport.
	Create bool
	// Get reports whether `get` on `pods/portforward` is granted, which is
	// what admits the websocket transport.
	Get bool
	// CreateReason and GetReason carry the API server's own explanation for
	// each verdict, verbatim, since that is what an operator debugs from.
	CreateReason string
	GetReason    string
}

// Allowed reports whether at least one transport is admitted — i.e. whether a
// dial can succeed at all, however it gets there.
func (a PortForwardAccess) Allowed() bool {
	return a.Create || a.Get
}

// PortForwardAllowed asks the API server, through two SelfSubjectAccessReviews
// (one per verb), whether this ServiceAccount may port-forward in the
// namespace over either transport.
//
// It is the difference between telling an admin "add `create` and/or `get` on
// `pods/portforward` to the Role" and making them read a 403 out of a stream
// upgrade failure. The reviews themselves need no RBAC: system:basic-user
// grants them to every authenticated identity, ServiceAccounts included.
func (t *KubernetesTunnel) PortForwardAllowed(ctx context.Context) (PortForwardAccess, error) {
	create, createReason, err := t.reviewPortForward(ctx, "create")
	if err != nil {
		return PortForwardAccess{}, err
	}

	get, getReason, err := t.reviewPortForward(ctx, "get")
	if err != nil {
		return PortForwardAccess{}, err
	}

	return PortForwardAccess{
		Create: create, Get: get,
		CreateReason: createReason, GetReason: getReason,
	}, nil
}

// reviewPortForward runs one SelfSubjectAccessReview for verb on
// `pods/portforward`.
func (t *KubernetesTunnel) reviewPortForward(ctx context.Context, verb string) (bool, string, error) {
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace:   t.namespace,
				Verb:        verb,
				Resource:    "pods",
				Subresource: "portforward",
			},
		},
	}

	result, err := t.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, "", t.classify(err, "selfsubjectaccessreview ("+verb+")")
	}

	return result.Status.Allowed, result.Status.Reason, nil
}

// DialPod opens one port-forward stream pair to podName's containerPort and
// returns it as a net.Conn.
//
// The context does not bound the dial — the SPDY/websocket upgrade below takes
// none — but it is still carried into the conn, whose error-stream drain logs a
// recovered panic against it.
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

		return nil, fmt.Errorf("%w: got %q, want %q", ErrKubernetesProtocolMismatch,
			protocol, portforward.PortForwardProtocolV1Name)
	}

	pfConn, err := newPortForwardConn(ctx, conn, t.namespace, podName, containerPort)
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
// that can honor a bastion.
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

	// A trust failure means something different depending on where the trust
	// came from. Against a pasted bundle it is "you pasted the wrong CA";
	// against a bundle we learned ourselves it is "the CA changed since we
	// pinned it", which is rotation or interception and needs its own name.
	if t.learnedPin && isCertificateTrustFailure(err) {
		return fmt.Errorf("%w (%s): %w", ErrKubernetesCAPinMismatch, what, err)
	}

	switch {
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w (%s): %w", ErrKubernetesUnauthorized, what, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w (%s): %w", ErrKubernetesForbidden, what, err)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w (%s): %w", ErrKubernetesTargetNotFound, what, err)
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "401 unauthorized"), strings.Contains(msg, "\"unauthorized\""):
		return fmt.Errorf("%w (%s): %w", ErrKubernetesUnauthorized, what, err)
	case strings.Contains(msg, "403 forbidden"), strings.Contains(msg, "is forbidden"):
		return fmt.Errorf("%w (%s): %w", ErrKubernetesForbidden, what, err)
	case strings.Contains(msg, "404 not found"), strings.Contains(msg, "not found"):
		return fmt.Errorf("%w (%s): %w", ErrKubernetesTargetNotFound, what, err)
	}

	return fmt.Errorf("kubernetes: %s failed: %w", what, err)
}

// isCertificateTrustFailure reports whether err is certificate verification
// failing, as opposed to any other transport problem.
//
// The typed x509 errors are matched first; the text fallback is there because
// the stream-upgrade paths wrap failures in their own error types often enough
// that the chain does not always survive intact.
func isCertificateTrustFailure(err error) bool {
	var (
		unknownAuthority x509.UnknownAuthorityError
		invalid          x509.CertificateInvalidError
		hostname         x509.HostnameError
	)

	if errors.As(err, &unknownAuthority) || errors.As(err, &invalid) || errors.As(err, &hostname) {
		return true
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "certificate signed by unknown authority") ||
		strings.Contains(msg, "x509:") ||
		strings.Contains(msg, "tls: failed to verify")
}
