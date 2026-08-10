package conncheck

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fclairamb/dbbat/internal/crypto"
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
	pod     *corev1.Pod
	slices  *discoveryv1.EndpointSliceList
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
		writeObjJSON(w, &authv1.SelfSubjectAccessReview{
			TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"},
			Status:   authv1.SubjectAccessReviewStatus{Allowed: f.allowed, Reason: f.reason},
		})
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

// TestCheckClusterUntrustedCertificate covers the mistake an operator makes
// first: pasting no CA bundle, or the wrong one.
func TestCheckClusterUntrustedCertificate(t *testing.T) {
	t.Parallel()

	fake := newFakeAPIServer(t)
	fake.allowed = true

	resolver := newFakeResolver()
	cluster := fake.newCluster(t, resolver)
	cluster.ProtocolData.Kubernetes.CACert = ""

	res := New(resolver, testKey()).Check(context.Background(), cluster)
	if res.OK {
		t.Fatal("Check() succeeded against an untrusted certificate")
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
