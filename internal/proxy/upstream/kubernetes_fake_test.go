package upstream

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
	apispdy "k8s.io/streaming/pkg/httpstream/spdy"
)

// fakeCluster is an in-process stand-in for a Kubernetes API server: it speaks
// just enough of the REST API (pods, endpointslices, SelfSubjectAccessReview)
// and, crucially, a real SPDY `pods/portforward` upgrade. httpstream is
// testable in-process, so the dialer and its net.Conn adapter are exercised
// against a genuine stream connection rather than a mock.
type fakeCluster struct {
	t   *testing.T
	srv *httptest.Server

	// portForwardStatus, when non-zero, fails the upgrade with that HTTP status
	// instead of switching protocols.
	portForwardStatus int
	// forwardError, when non-empty, is written on the error stream and the data
	// stream is closed — how a kubelet reports "nothing is listening on that
	// container port".
	forwardError string

	slices  *discoveryv1.EndpointSliceList
	pod     *corev1.Pod
	allowed bool
	reason  string
	// allowedByVerb overrides allowed/reason for one RBAC verb ("create" or
	// "get") when present; a verb absent from the map falls back to
	// allowed/reason, so most tests can keep answering both verbs alike.
	allowedByVerb map[string]bool
	reasonByVerb  map[string]string

	mu           sync.Mutex
	authHeaders  []string
	portHeaders  []string
	requestPaths []string
	userAgents   []string
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()

	f := &fakeCluster{t: t}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)

	return f
}

// caPEM returns the fake API server's certificate as a PEM bundle, which is
// what a cluster row's ca_cert field holds.
func (f *fakeCluster) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

// config builds a KubernetesConfig pointed at this fake, in the shape the store
// would produce for a cluster row.
func (f *fakeCluster) config() KubernetesConfig {
	host := strings.TrimPrefix(f.srv.URL, "https://")

	return KubernetesConfig{
		Host:      "https://" + host,
		Token:     "sa-token",
		CACert:    f.caPEM(),
		Namespace: "data",
	}
}

func (f *fakeCluster) tunnel(t *testing.T) *KubernetesTunnel {
	t.Helper()

	tunnel, err := NewKubernetesTunnel(f.config())
	if err != nil {
		t.Fatalf("NewKubernetesTunnel() error = %v", err)
	}

	return tunnel
}

func (f *fakeCluster) record(req *http.Request) {
	f.mu.Lock()
	f.authHeaders = append(f.authHeaders, req.Header.Get("Authorization"))
	f.requestPaths = append(f.requestPaths, req.URL.Path)
	f.userAgents = append(f.userAgents, req.Header.Get("User-Agent"))
	f.mu.Unlock()
}

func (f *fakeCluster) seenPorts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.portHeaders...)
}

func (f *fakeCluster) seenAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.authHeaders...)
}

func (f *fakeCluster) seenUserAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.userAgents...)
}

func (f *fakeCluster) route(w http.ResponseWriter, req *http.Request) {
	f.record(req)

	switch {
	case strings.HasSuffix(req.URL.Path, "/portforward"):
		f.handlePortForward(w, req)
	case strings.Contains(req.URL.Path, "/endpointslices"):
		f.writeJSON(w, f.endpointSliceList())
	case strings.Contains(req.URL.Path, "/selfsubjectaccessreviews"):
		f.handleSelfSubjectAccessReview(w, req)
	case strings.Contains(req.URL.Path, "/pods/"):
		if f.pod == nil {
			f.writeStatus(w, http.StatusNotFound, "pods \"x\" not found")

			return
		}
		f.writeJSON(w, f.pod)
	default:
		f.writeStatus(w, http.StatusNotFound, "unhandled path "+req.URL.Path)
	}
}

// handleSelfSubjectAccessReview answers per the requested verb: PortForwardAllowed
// now sends one review each for "create" and "get", and a test that wants them
// to disagree sets allowedByVerb/reasonByVerb rather than the plain
// allowed/reason fields, which answer identically for every verb.
func (f *fakeCluster) handleSelfSubjectAccessReview(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		f.t.Errorf("fake cluster: read selfsubjectaccessreview body: %v", err)

		return
	}

	// The typed clientset negotiates protobuf for built-in types like this one
	// (magic-prefixed "k8s\x00..."), not JSON, so the universal deserializer —
	// which recognizes either — is what has to read it back.
	var review authv1.SelfSubjectAccessReview
	if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
		f.t.Errorf("fake cluster: decode selfsubjectaccessreview: %v", err)

		return
	}

	var verb string
	if review.Spec.ResourceAttributes != nil {
		verb = review.Spec.ResourceAttributes.Verb
	}

	allowed, reason := f.allowed, f.reason
	if v, ok := f.allowedByVerb[verb]; ok {
		allowed = v
		reason = f.reasonByVerb[verb]
	}

	f.writeJSON(w, &authv1.SelfSubjectAccessReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"},
		Status:   authv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: reason},
	})
}

func (f *fakeCluster) endpointSliceList() *discoveryv1.EndpointSliceList {
	if f.slices != nil {
		return f.slices
	}

	return &discoveryv1.EndpointSliceList{
		TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSliceList"},
	}
}

func (f *fakeCluster) writeJSON(w http.ResponseWriter, obj any) {
	body, err := json.Marshal(obj)
	if err != nil {
		f.t.Errorf("fake cluster: marshal: %v", err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (f *fakeCluster) writeStatus(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	f.writeStatusBody(w, code, message)
}

func (f *fakeCluster) writeStatusBody(w io.Writer, code int, message string) {
	body, _ := json.Marshal(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Code:     int32(code),
	})
	_, _ = w.Write(body)
}

// handlePortForward performs the real SPDY upgrade and serves the stream pairs
// the client creates over it.
func (f *fakeCluster) handlePortForward(w http.ResponseWriter, req *http.Request) {
	if f.portForwardStatus != 0 {
		f.writeStatus(w, f.portForwardStatus, "portforward refused by the fake cluster")

		return
	}

	// The websocket-first dialer probes with GET; refusing it is what makes the
	// fallback to SPDY (the POST below) happen, exactly as against an API
	// server too old to tunnel SPDY over websockets.
	if req.Method != http.MethodPost {
		f.writeStatus(w, http.StatusBadRequest, "websocket tunneling not supported")

		return
	}

	if _, err := httpstream.Handshake(req, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
		f.writeStatus(w, http.StatusBadRequest, err.Error())

		return
	}

	streams := make(chan httpstream.Stream, 8)

	conn := apispdy.NewResponseUpgrader().UpgradeResponse(w, req,
		func(stream httpstream.Stream, _ <-chan struct{}) error {
			streams <- stream

			return nil
		})
	if conn == nil {
		return
	}

	defer func() { _ = conn.Close() }()

	go f.servePairs(streams)

	<-conn.CloseChan()
}

// servePairs pairs each data stream with the error stream sharing its request
// id — the same requestID bookkeeping the real kubelet does.
func (f *fakeCluster) servePairs(streams <-chan httpstream.Stream) {
	pending := map[string]httpstream.Stream{}

	for stream := range streams {
		id := stream.Headers().Get(corev1.PortForwardRequestIDHeader)

		switch stream.Headers().Get(corev1.StreamType) {
		case corev1.StreamTypeError:
			pending[id] = stream
		case corev1.StreamTypeData:
			errStream := pending[id]
			delete(pending, id)

			f.mu.Lock()
			f.portHeaders = append(f.portHeaders, stream.Headers().Get(corev1.PortHeader))
			f.mu.Unlock()

			go f.servePair(stream, errStream)
		}
	}
}

func (f *fakeCluster) servePair(data, errStream httpstream.Stream) {
	if f.forwardError != "" {
		if errStream != nil {
			_, _ = errStream.Write([]byte(f.forwardError))
			_ = errStream.Close()
		}
		_ = data.Close()

		return
	}

	// Echo: the simplest upstream that proves bytes survive the round trip.
	_, _ = io.Copy(data, data)
	_ = data.Close()

	if errStream != nil {
		_ = errStream.Close()
	}
}

// readyEndpointSlices builds an EndpointSliceList whose first endpoint is not
// ready and whose second is, so a resolver that ignores readiness picks wrong.
func readyEndpointSlices(notReadyPod, readyPod string) *discoveryv1.EndpointSliceList {
	no, yes := false, true

	return &discoveryv1.EndpointSliceList{
		TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSliceList"},
		Items: []discoveryv1.EndpointSlice{{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-abc", Namespace: "data"},
			Endpoints: []discoveryv1.Endpoint{
				{
					Conditions: discoveryv1.EndpointConditions{Ready: &no},
					TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: notReadyPod},
				},
				{
					Conditions: discoveryv1.EndpointConditions{Ready: &yes},
					TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: readyPod},
				},
			},
		}},
	}
}
