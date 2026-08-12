package conncheck

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
)

// fakeAPIServer answers the three questions the Kubernetes classifier asks —
// may I port-forward, does the target resolve, is it ready — over TLS, so the
// pasted CA bundle is exercised too. It does not implement the port-forward
// upgrade: that is covered against a real SPDY endpoint in
// internal/proxy/upstream.
type fakeAPIServer struct {
	srv *httptest.Server

	allowed bool
	reason  string
	// allowedByVerb overrides allowed/reason for one RBAC verb ("create" or
	// "get") when present; a verb absent from the map falls back to
	// allowed/reason, so most tests can keep answering both verbs alike.
	allowedByVerb map[string]bool
	reasonByVerb  map[string]string
	pod           *corev1.Pod
	slices        *discoveryv1.EndpointSliceList
	// status, when non-zero, is returned for every request.
	status int
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()

	f := &fakeAPIServer{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeAPIServer) route(w http.ResponseWriter, req *http.Request) {
	if f.status != 0 {
		writeStatusJSON(w, f.status, "refused by the fake API server")

		return
	}

	switch {
	case strings.Contains(req.URL.Path, "/selfsubjectaccessreviews"):
		f.handleSelfSubjectAccessReview(w, req)
	case strings.Contains(req.URL.Path, "/endpointslices"):
		if f.slices == nil {
			writeObjJSON(w, &discoveryv1.EndpointSliceList{
				TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSliceList"},
			})

			return
		}
		writeObjJSON(w, f.slices)
	case strings.Contains(req.URL.Path, "/pods/"):
		if f.pod == nil {
			writeStatusJSON(w, http.StatusNotFound, "pods not found")

			return
		}
		writeObjJSON(w, f.pod)
	default:
		writeStatusJSON(w, http.StatusNotFound, "unhandled "+req.URL.Path)
	}
}

// handleSelfSubjectAccessReview answers per the requested verb: checkCluster
// now sends one review each for "create" and "get", and a test that wants
// them to disagree sets allowedByVerb/reasonByVerb rather than the plain
// allowed/reason fields, which answer identically for every verb.
func (f *fakeAPIServer) handleSelfSubjectAccessReview(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeStatusJSON(w, http.StatusBadRequest, "could not read the request body")

		return
	}

	// The typed clientset negotiates protobuf for built-in types like this one
	// (magic-prefixed "k8s\x00..."), not JSON, so the universal deserializer —
	// which recognizes either — is what has to read it back.
	var review authv1.SelfSubjectAccessReview
	if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
		writeStatusJSON(w, http.StatusBadRequest, "could not decode the request body")

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

	writeObjJSON(w, &authv1.SelfSubjectAccessReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"},
		Status:   authv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: reason},
	})
}

func writeObjJSON(w http.ResponseWriter, obj any) {
	body, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func writeStatusJSON(w http.ResponseWriter, code int, message string) {
	body, _ := json.Marshal(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Code:     int32(code),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// unrelatedCAPEM mints a throwaway self-signed CA. It has to be generated
// rather than borrowed from a second httptest server: every httptest TLS
// server presents the same built-in certificate, so two of them would "verify"
// against each other and the test would pass for the wrong reason.
func unrelatedCAPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not-your-cluster"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// newCluster registers a `protocol: kubernetes` row pointed at f, with its
// ServiceAccount token encrypted the way the store stores it.
func (f *fakeAPIServer) newCluster(t *testing.T, r *fakeResolver) *store.Server {
	t.Helper()

	uid := uuid.New()

	token, err := crypto.Encrypt([]byte("sa-token"), testKey(), crypto.ServerAAD(uid.String()))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}

	host, port := splitHostPort(t, strings.TrimPrefix(f.srv.URL, "https://"))
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))

	srv := &store.Server{
		UID: uid, Host: host, Port: port,
		Protocol:          store.ProtocolKubernetes,
		PasswordEncrypted: token,
		ProtocolData: &store.ServerProtocolData{
			Kubernetes: &store.KubernetesServerData{CACert: caPEM, Namespace: "data"},
		},
	}
	r.servers[uid] = srv

	return srv
}

func TestCheckClusterAllowed(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res.OK {
		t.Fatalf("Check() = %+v, want OK", res)
	}
	if res.Stage != StageClusterRBAC || res.Code != CodeOK {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeOK)
	}
	if !strings.Contains(res.Message, "data") {
		t.Errorf("message %q does not name the namespace", res.Message)
	}
}

func TestCheckClusterMissingRBAC(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = false
	fake.reason = "no RBAC policy matched"

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded despite a denied SelfSubjectAccessReview")
	}
	if res.Stage != StageClusterRBAC || res.Code != CodeK8sForbidden {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeK8sForbidden)
	}
	// The whole point of asking explicitly is naming the missing verb.
	if !strings.Contains(res.Message, "pods/portforward") {
		t.Errorf("message %q does not name the missing verb", res.Message)
	}
	if !strings.Contains(res.Message, "no RBAC policy matched") {
		t.Errorf("message %q drops the API server's reason", res.Message)
	}
}

// TestCheckClusterCreateOnlyRoleSucceedsWithACostNote covers the Role that
// grants `create` but not `get`: the check must still pass — the SPDY
// transport carries every dial — but the message has to say the websocket
// upgrade is refused first on each one.
func TestCheckClusterCreateOnlyRoleSucceedsWithACostNote(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowedByVerb = map[string]bool{"create": true, "get": false}

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res.OK {
		t.Fatalf("Check() = %+v, want OK: the SPDY transport still works with `create` alone", res)
	}
	if res.Stage != StageClusterRBAC || res.Code != CodeOK {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeOK)
	}
	if !strings.Contains(res.Message, "SPDY") || !strings.Contains(res.Message, "websocket") {
		t.Errorf("message %q does not explain the refused-GET-then-fallback cost of a create-only Role", res.Message)
	}
}

// TestCheckClusterGetOnlyRoleSucceedsWithACostNote covers the Role that grants
// `get` but not `create`: it succeeds too (the websocket transport carries the
// dial), but the message must warn that this cannot work at all against a
// pre-1.31 API server.
func TestCheckClusterGetOnlyRoleSucceedsWithACostNote(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowedByVerb = map[string]bool{"create": false, "get": true}

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res.OK {
		t.Fatalf("Check() = %+v, want OK: the websocket transport still works with `get` alone", res)
	}
	if res.Stage != StageClusterRBAC || res.Code != CodeOK {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeOK)
	}
	if !strings.Contains(res.Message, "1.31") {
		t.Errorf("message %q does not warn about pre-1.31 API servers", res.Message)
	}
}

// TestCheckClusterNeitherVerbGrantedFails is the fourth combination: with
// neither verb granted, the check must fail cluster_rbac and name both verbs
// so an admin can grant whichever transport they want.
func TestCheckClusterNeitherVerbGrantedFails(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowedByVerb = map[string]bool{"create": false, "get": false}
	fake.reasonByVerb = map[string]string{
		"create": "no policy grants create",
		"get":    "no policy grants get",
	}

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded although neither verb is granted")
	}
	if res.Stage != StageClusterRBAC || res.Code != CodeK8sForbidden {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeK8sForbidden)
	}
	if !strings.Contains(res.Message, "create") || !strings.Contains(res.Message, "get") {
		t.Errorf("message %q does not name both verbs", res.Message)
	}
	if !strings.Contains(res.Message, "no policy grants create") || !strings.Contains(res.Message, "no policy grants get") {
		t.Errorf("message %q drops the per-verb reasons", res.Message)
	}
}

func TestCheckClusterRejectedToken(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.status = http.StatusUnauthorized

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded despite a 401")
	}
	if res.Stage != StageClusterAuth || res.Code != CodeAuthRejected {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterAuth, CodeAuthRejected)
	}
}

func TestCheckClusterForbiddenReview(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.status = http.StatusForbidden

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.Stage != StageClusterRBAC || res.Code != CodeK8sForbidden {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeK8sForbidden)
	}
}

func TestCheckClusterWithoutAToken(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	uid := uuid.New()
	cluster := &store.Server{
		UID: uid, Host: "api.example.invalid", Port: 6443, Protocol: store.ProtocolKubernetes,
	}
	resolver.servers[uid] = cluster

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.Stage != StageConfig || res.Code != CodeNoAuthMethod {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageConfig, CodeNoAuthMethod)
	}
}

// TestCheckClusterWithoutACABundleFailsClosed: a row that pins no CA, opted out
// of nothing, and cannot be reached has nothing to learn from — so it must be
// refused, never quietly verified against the host's system roots.
func TestCheckClusterWithoutACABundleFailsClosed(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	uid := uuid.New()

	token, err := crypto.Encrypt([]byte("sa-token"), testKey(), crypto.ServerAAD(uid.String()))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}

	cluster := &store.Server{
		UID: uid, Host: "127.0.0.1", Port: 1, Protocol: store.ProtocolKubernetes,
		PasswordEncrypted: token,
		ProtocolData: &store.ServerProtocolData{
			Kubernetes: &store.KubernetesServerData{Namespace: "data"},
		},
	}
	resolver.servers[uid] = cluster

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded on a row that pins no CA, learned none, and did not opt out")
	}

	if cluster.ProtocolData.Kubernetes.LearnedCACert != "" {
		t.Error("a CA was pinned although the capture could not happen")
	}
}

// TestCheckClusterPinsTheCAOnFirstConnect is the TOFU flow as an operator meets
// it: a cluster row with no pasted bundle connects, the check reports that
// *this* run performed the pin, and the second check does not claim it again.
func TestCheckClusterPinsTheCAOnFirstConnect(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	wantPEM := cluster.ProtocolData.Kubernetes.CACert
	cluster.ProtocolData.Kubernetes.CACert = ""

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res.OK {
		t.Fatalf("Check() = %+v, want OK with the CA learned on first connect", res)
	}

	if !res.CAPinned {
		t.Error("CAPinned = false, want true on the first connect of a row with no pasted bundle")
	}

	if res.LearnedCACert != wantPEM {
		t.Errorf("LearnedCACert = %q, want the API server's certificate %q", res.LearnedCACert, wantPEM)
	}

	if !strings.Contains(res.Message, "pinned") {
		t.Errorf("message %q does not mention the pin", res.Message)
	}

	res2 := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res2.OK {
		t.Fatalf("second Check() = %+v, want OK against the learned pin", res2)
	}

	if res2.CAPinned {
		t.Error("CAPinned = true on the second check, want false")
	}
}

// TestCheckClusterCAPinMismatch is binding semantic #5: after pinning, a
// certificate the pin does not vouch for must be refused *and* named apart from
// "you pasted the wrong CA", because rotation and interception look identical
// on the wire and only an operator can tell them apart.
func TestCheckClusterCAPinMismatch(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	cluster.ProtocolData.Kubernetes.CACert = ""
	cluster.ProtocolData.Kubernetes.LearnedCACert = unrelatedCAPEM(t)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded against a certificate the pin does not vouch for")
	}

	if res.Code != CodeK8sCAPinMismatch {
		t.Fatalf("code = %s, want %s (message: %s)", res.Code, CodeK8sCAPinMismatch, res.Message)
	}

	if res.Stage != StageClusterAPI {
		t.Errorf("stage = %s, want %s", res.Stage, StageClusterAPI)
	}

	// The exit from a stale pin is the whole reason TOFU was worth adding.
	if !strings.Contains(res.Message, "reset") || !strings.Contains(res.Message, "rotated") {
		t.Errorf("message %q does not offer the rotation exit", res.Message)
	}
}

// TestCheckClusterSuppliedCAWinsOverALearnedOne is binding semantic #2 at the
// check layer: a stale learned value must not shadow the bundle an operator
// pasted.
func TestCheckClusterSuppliedCAWinsOverALearnedOne(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	cluster.ProtocolData.Kubernetes.LearnedCACert = unrelatedCAPEM(t)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res.OK {
		t.Fatalf("Check() = %+v, want the supplied bundle to be the one in force", res)
	}

	if res.CAPinned {
		t.Error("CAPinned = true although the row supplied its own bundle")
	}
}

// TestClassifyMissingCAConfiguration keeps the fail-closed branch of the
// constructor covered even though the dialer now tries a TOFU capture first: a
// row can still reach it (a hand-edited database, a restore, an import path).
func TestClassifyMissingCAConfiguration(t *testing.T) {
	t.Parallel()

	res := classifyKubernetesError(upstream.ErrKubernetesNoCACert, StageClusterAPI)
	if res.Stage != StageConfig || res.Code != CodeMissingConfig {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageConfig, CodeMissingConfig)
	}

	if !strings.Contains(res.Message, "system trust store") {
		t.Errorf("message %q drops the reason dbbat refuses rather than falling back", res.Message)
	}
}

// TestCheckClusterWithTheInsecureOptOut pins that the escape hatch actually
// works: the fake's certificate is untrusted, and the row still connects.
func TestCheckClusterWithTheInsecureOptOut(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	cluster.ProtocolData.Kubernetes.CACert = ""
	cluster.ProtocolData.Kubernetes.InsecureSkipTLSVerify = true

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if !res.OK {
		t.Fatalf("Check() = %+v, want OK with verification skipped", res)
	}
}

// TestCheckClusterWrongCABundle is the ordinary operator mistake: a CA is
// pinned, but not the one that signed the API server's certificate.
func TestCheckClusterWrongCABundle(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	cluster.ProtocolData.Kubernetes.CACert = unrelatedCAPEM(t)

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded against a certificate signed by a different CA")
	}
	if !strings.Contains(res.Message, "CA") {
		t.Errorf("message %q does not point at the CA certificate field", res.Message)
	}
}

func TestCheckClusterUnreachableAPIServer(t *testing.T) {
	t.Parallel()

	resolver := newFakeResolver()
	uid := uuid.New()

	token, err := crypto.Encrypt([]byte("sa-token"), testKey(), crypto.ServerAAD(uid.String()))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}

	cluster := &store.Server{
		UID: uid, Host: "127.0.0.1", Port: 1, Protocol: store.ProtocolKubernetes,
		PasswordEncrypted: token,
		// Trust is configured, so the check gets far enough to be about the
		// network rather than stopping at the config stage.
		ProtocolData: &store.ServerProtocolData{
			Kubernetes: &store.KubernetesServerData{Namespace: "data", InsecureSkipTLSVerify: true},
		},
	}
	resolver.servers[uid] = cluster

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded against an unreachable API server")
	}
	if res.Stage != StageClusterAPI {
		t.Errorf("stage = %s, want %s", res.Stage, StageClusterAPI)
	}
}

// newKubernetesTarget registers a database row reached through cluster.
func newKubernetesTarget(r *fakeResolver, cluster *store.Server, host string) *store.Server {
	uid := uuid.New()
	srv := &store.Server{
		UID: uid, Host: host, Port: 5432,
		Protocol: store.ProtocolPostgreSQL, DatabaseName: "app",
		Username: "app", ViaUID: &cluster.UID,
	}
	r.servers[uid] = srv

	return srv
}

func TestCheckTargetThroughClusterUnknownPod(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	target := newKubernetesTarget(resolver, cluster, "pg-0")

	res := New(resolver, testKey()).Check(context.Background(), target)
	if res.OK {
		t.Fatal("Check() succeeded against a pod that does not exist")
	}
	if res.Stage != StageClusterTarget || res.Code != CodeK8sTargetNotFound {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterTarget, CodeK8sTargetNotFound)
	}
	// The scope limitation is the thing operators get wrong; say it here.
	if !strings.Contains(res.Message, "svc/") {
		t.Errorf("message %q does not explain the pod/service addressing", res.Message)
	}
}

func TestCheckTargetThroughClusterPodNotReady(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true
	fake.pod = &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-0", Namespace: "data"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	target := newKubernetesTarget(resolver, cluster, "pg-0")

	res := New(resolver, testKey()).Check(context.Background(), target)
	if res.Stage != StageClusterTarget || res.Code != CodeK8sTargetNotReady {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterTarget, CodeK8sTargetNotReady)
	}
}

// TestCheckTargetThroughClusterReportsRBACNotTheDatabase pins the ordering that
// makes the staged result useful: a missing verb must not be reported as the
// database refusing the connection.
func TestCheckTargetThroughClusterReportsRBACNotTheDatabase(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = false

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	target := newKubernetesTarget(resolver, cluster, "pg-0")

	res := New(resolver, testKey()).Check(context.Background(), target)
	if res.Stage != StageClusterRBAC || res.Code != CodeK8sForbidden {
		t.Errorf("stage/code = %s/%s, want %s/%s", res.Stage, res.Code, StageClusterRBAC, CodeK8sForbidden)
	}
}

func TestCheckTargetThroughClusterResolvesAService(t *testing.T) {
	t.Parallel()

	ready := true
	fake := newFakeAPIServer(t)
	fake.allowed = true
	fake.slices = &discoveryv1.EndpointSliceList{
		TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSliceList"},
		Items: []discoveryv1.EndpointSlice{{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-abc", Namespace: "data"},
			Endpoints: []discoveryv1.Endpoint{{
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: "pg-7"},
			}},
		}},
	}
	// The resolved pod is deliberately absent, so the failure names it.
	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	target := newKubernetesTarget(resolver, cluster, "svc/postgres")

	res := New(resolver, testKey()).Check(context.Background(), target)
	if res.Stage != StageClusterTarget {
		t.Fatalf("stage = %s, want %s (%s)", res.Stage, StageClusterTarget, res.Message)
	}
	if !strings.Contains(res.Message, "svc/postgres") && res.Code != CodeK8sTargetNotFound {
		t.Errorf("message %q should trace back to the service the admin typed", res.Message)
	}
}
