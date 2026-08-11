//go:build integration

package kubernetes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	"github.com/testcontainers/testcontainers-go/wait"
	"gopkg.in/yaml.v3"
)

// k3sStartupTimeout bounds the cluster boot. It is generous on purpose: a cold
// run pulls a ~250MB k3s image before the API server exists at all.
const k3sStartupTimeout = 10 * time.Minute

// defaultK3sImage is the cluster the suite runs against.
//
// It must be **1.31 or newer**: KEP-4006 (SPDY-over-websocket port forwarding)
// is only beta-and-on-by-default from 1.31, and the whole point of running
// against a real API server is to exercise the websocket transport that the
// in-process fake cannot serve. Override with K3S_TEST_IMAGE to run the same
// suite against another version — a pre-1.31 image is a legitimate thing to
// point it at, but every dial will then take the SPDY fallback and
// TestIntegration_K8s_WebsocketTransport will fail, which is the correct
// answer rather than a flake.
const defaultK3sImage = "rancher/k3s:v1.33.13-k3s1"

// upstreamPGImage is the database that runs *inside* the cluster. A port
// forward reaches a pod's own port, so this is the only shape of upstream the
// tunnel can address at all (docs/kubernetes.md, "Scope").
const upstreamPGImage = "postgres:15-alpine"

const (
	// testNamespace is the namespace the Roles below are scoped to. The Role is
	// namespace-scoped on purpose: one namespace per cluster row is the
	// granularity the model is built around.
	testNamespace = "data"
	// pgService is the Service in front of the PostgreSQL Deployment; the
	// database row addresses it as `svc/dbbat-pg`.
	pgService = "dbbat-pg"
	// pgContainerPort is the *container* port. A port-forward terminates in the
	// kubelet and speaks to the pod directly, so a Service's port mapping is
	// never applied.
	pgContainerPort = 5432

	upstreamDB   = "testdb"
	upstreamUser = "postgres"
	upstreamPass = "postgres"
)

// The three ServiceAccounts the suite authenticates as. They differ *only* in
// the verbs their Role carries on `pods/portforward`, which is what makes them
// a transport discriminator as well as an RBAC fixture:
//
//   - the API server derives the RBAC verb from the HTTP method (GET → `get`,
//     POST → `create`, see k8s.io/apiserver requestinfo.go);
//   - the websocket upgrade is a GET (RFC 6455 §4.1) and the SPDY upgrade is a
//     POST.
//
// So a dial that succeeds as saPortForwardGet can only have gone over the
// websocket transport, and one that succeeds as saPortForwardCreate can only
// have gone over the SPDY fallback. No instrumentation of client-go's opaque
// fallback dialer — and no API server audit log — is needed to tell them apart.
//
// Both are therefore *deliberately narrower* than the Role docs/kubernetes.md
// recommends, which grants `create` **and** `get` precisely so neither
// transport is refused. Granting both here would make every dial ambiguous and
// destroy the discriminator, so the fixture splits the recommended Role in two
// and each half is exercised on its own.
const (
	// saPortForwardCreate carries `create` on pods/portforward only — the
	// documented Role minus its `get` verb, i.e. what a deployment written
	// against the pre-1.31 advice still has. Only SPDY can satisfy it.
	saPortForwardCreate = "dbbat"
	// saPortForwardGet carries `get` on pods/portforward only — the other half.
	// Only the websocket upgrade can satisfy it.
	saPortForwardGet = "dbbat-ws"
	// saNoPortForward carries no pods/portforward verb at all — the "somebody
	// forgot the Role" case the connectivity check exists to name.
	saNoPortForward = "dbbat-norbac"
)

func k3sImage() string {
	if img := os.Getenv("K3S_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultK3sImage
}

// clusterManifests is everything applied to the cluster in one shot: the
// namespace, the three ServiceAccount/Secret/Role/RoleBinding bundles, and the
// PostgreSQL Deployment + Service the tunnel forwards into.
//
// The token Secrets are explicit `kubernetes.io/service-account-token` objects
// because Kubernetes 1.24+ no longer auto-creates them and projected tokens
// expire — the same reason docs/kubernetes.md tells an operator to ask for one.
const clusterManifests = `
apiVersion: v1
kind: Namespace
metadata:
  name: data
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dbbat
  namespace: data
---
apiVersion: v1
kind: Secret
metadata:
  name: dbbat-token
  namespace: data
  annotations:
    kubernetes.io/service-account.name: dbbat
type: kubernetes.io/service-account-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dbbat-portforward
  namespace: data
rules:
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: dbbat-portforward
  namespace: data
subjects:
  - kind: ServiceAccount
    name: dbbat
    namespace: data
roleRef:
  kind: Role
  name: dbbat-portforward
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dbbat-ws
  namespace: data
---
apiVersion: v1
kind: Secret
metadata:
  name: dbbat-ws-token
  namespace: data
  annotations:
    kubernetes.io/service-account.name: dbbat-ws
type: kubernetes.io/service-account-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dbbat-ws-portforward
  namespace: data
rules:
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: dbbat-ws-portforward
  namespace: data
subjects:
  - kind: ServiceAccount
    name: dbbat-ws
    namespace: data
roleRef:
  kind: Role
  name: dbbat-ws-portforward
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dbbat-norbac
  namespace: data
---
apiVersion: v1
kind: Secret
metadata:
  name: dbbat-norbac-token
  namespace: data
  annotations:
    kubernetes.io/service-account.name: dbbat-norbac
type: kubernetes.io/service-account-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dbbat-norbac
  namespace: data
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: dbbat-norbac
  namespace: data
subjects:
  - kind: ServiceAccount
    name: dbbat-norbac
    namespace: data
roleRef:
  kind: Role
  name: dbbat-norbac
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dbbat-pg
  namespace: data
spec:
  replicas: 1
  selector:
    matchLabels:
      app: dbbat-pg
  template:
    metadata:
      labels:
        app: dbbat-pg
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: postgres
          image: postgres:15-alpine
          env:
            - name: POSTGRES_DB
              value: testdb
            - name: POSTGRES_USER
              value: postgres
            - name: POSTGRES_PASSWORD
              value: postgres
            - name: PGDATA
              value: /var/lib/postgresql/data/pgdata
          ports:
            - containerPort: 5432
          readinessProbe:
            # Deliberately over TCP rather than the unix socket: the image's
            # entrypoint runs a temporary server with listen_addresses='' while
            # initdb runs, so a socket-based pg_isready reports ready before the
            # database is reachable through a port-forward.
            exec:
              command: ["pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "testdb"]
            initialDelaySeconds: 2
            periodSeconds: 2
            failureThreshold: 60
---
apiVersion: v1
kind: Service
metadata:
  name: dbbat-pg
  namespace: data
spec:
  selector:
    app: dbbat-pg
  ports:
    - name: pg
      port: 5432
      targetPort: 5432
`

// k3sCluster is the started cluster plus everything a dbbat cluster row needs
// to reach it.
type k3sCluster struct {
	container *k3s.K3sContainer
	// apiHost/apiPort are the API server as reachable from *this* process — the
	// container's mapped 6443. They come out of the kubeconfig rather than out
	// of MappedPort so the host matches the `--tls-san` k3s was started with,
	// and therefore the certificate the CA below vouches for.
	apiHost string
	apiPort int
	// caPEM is the cluster CA bundle. This and the token are exactly the two
	// values docs/kubernetes.md tells an operator to read out of the cluster.
	caPEM string
}

// saCredentials is what one ServiceAccount's token Secret yields: the bearer
// token dbbat authenticates with, and the CA bundle it verifies the API server
// against.
type saCredentials struct {
	token string
	caPEM string
}

// startK3s brings up the cluster, applies the manifests, and waits for the
// PostgreSQL pod to be Ready.
//
// It returns an error rather than taking a *testing.T because it runs from
// TestMain: one cluster is shared by the whole suite (booting k3s costs a
// minute or two), while each test still gets its own dbbat store container.
func startK3s(ctx context.Context) (*k3sCluster, error) {
	container, err := k3s.Run(ctx, k3sImage(),
		// Same signal the module waits on by default, with a budget that
		// survives a cold image pull.
		testcontainers.WithWaitStrategy(
			wait.ForLog(".*Node controller sync successful.*").AsRegexp().
				WithStartupTimeout(k3sStartupTimeout),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start k3s (%s): %w", k3sImage(), err)
	}

	cluster := &k3sCluster{container: container}

	if err := cluster.readKubeconfig(ctx); err != nil {
		return cluster, err
	}

	if err := cluster.apply(ctx, clusterManifests); err != nil {
		return cluster, err
	}

	if err := cluster.waitForReadyPod(ctx, "", 10*time.Minute); err != nil {
		return cluster, fmt.Errorf("waiting for the postgres pod: %w", err)
	}

	return cluster, nil
}

// terminate tears the cluster down. Only what this suite created is removed —
// testcontainers owns the lifecycle end to end.
func (c *k3sCluster) terminate(ctx context.Context) {
	if c == nil || c.container == nil {
		return
	}

	_ = c.container.Terminate(ctx)
}

// readKubeconfig derives the API server address and CA bundle from the
// kubeconfig k3s emits.
//
// **This is the one and only place a kubeconfig is parsed.** dbbat itself
// deliberately does not accept one (docs/kubernetes.md: the kubeconfigs EKS,
// GKE and AKS generate authenticate through exec credential plugins, which a
// server daemon cannot run), so the parsing lives here, in the test, and what
// crosses into product code is only the three values a cluster row holds:
// host, port and CA bundle — plus the token, which is minted separately from
// the ServiceAccount's token Secret.
func (c *k3sCluster) readKubeconfig(ctx context.Context) error {
	raw, err := c.container.GetKubeConfig(ctx)
	if err != nil {
		return fmt.Errorf("read the k3s kubeconfig: %w", err)
	}

	var parsed k3s.KubeConfigValue
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse the k3s kubeconfig: %w", err)
	}

	if len(parsed.Clusters) == 0 {
		return errors.New("the k3s kubeconfig names no cluster")
	}

	server := parsed.Clusters[0].Cluster.Server

	endpoint, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("parse the API server URL %q: %w", server, err)
	}

	port, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		return fmt.Errorf("the API server URL %q carries no port: %w", server, err)
	}

	ca, err := base64.StdEncoding.DecodeString(parsed.Clusters[0].Cluster.CertificateAuthorityData)
	if err != nil {
		return fmt.Errorf("decode the cluster CA bundle: %w", err)
	}

	c.apiHost = endpoint.Hostname()
	c.apiPort = port
	c.caPEM = string(ca)

	return nil
}

// apply writes a manifest into the container and applies it with kubectl.
//
// kubectl comes from the k3s binary itself (the image symlinks it), so the
// cluster is driven with the same tool an operator would use and this package
// never imports client-go — which stays confined to
// internal/proxy/upstream/kubernetes*.go.
func (c *k3sCluster) apply(ctx context.Context, manifest string) error {
	const path = "/tmp/dbbat-manifests.yaml"

	if err := c.container.CopyToContainer(ctx, []byte(manifest), path, 0o644); err != nil {
		return fmt.Errorf("copy the manifests into the k3s container: %w", err)
	}

	out, code, err := c.exec(ctx, "kubectl", "apply", "-f", path)
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("kubectl apply exited %d: %s", code, out) //nolint:err113 // test diagnostic
	}

	return nil
}

// exec runs a command inside the k3s container and returns its combined output
// and exit code.
func (c *k3sCluster) exec(ctx context.Context, args ...string) (string, int, error) {
	code, reader, err := c.container.Exec(ctx, args,
		tcexec.Multiplexed(),
		tcexec.WithEnv([]string{"KUBECONFIG=/etc/rancher/k3s/k3s.yaml"}),
	)
	if err != nil {
		return "", code, fmt.Errorf("exec %v in the k3s container: %w", args, err)
	}

	out, err := io.ReadAll(reader)
	if err != nil {
		return "", code, fmt.Errorf("read the output of %v: %w", args, err)
	}

	return string(out), code, nil
}

// kubectl runs kubectl inside the cluster and fails the test on a non-zero
// exit.
func (c *k3sCluster) kubectl(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()

	out, code, err := c.exec(ctx, append([]string{"kubectl"}, args...)...)
	if err != nil {
		t.Fatalf("kubectl %v: %v", args, err)
	}

	if code != 0 {
		t.Fatalf("kubectl %v exited %d: %s", args, code, out)
	}

	return strings.TrimSpace(out)
}

// serviceAccountToken mints (well: reads) the bearer token and CA bundle of one
// ServiceAccount, from the token Secret created for it — the exact two
// `kubectl get secret -o jsonpath` reads docs/kubernetes.md prescribes.
//
// The Secret is populated asynchronously by the token controller, so this
// polls rather than assuming it is there the moment `kubectl apply` returns.
func (c *k3sCluster) serviceAccountToken(ctx context.Context, t *testing.T, name string) saCredentials {
	t.Helper()

	secret := name + "-token"
	deadline := time.Now().Add(2 * time.Minute)

	for {
		out, code, err := c.exec(ctx,
			"kubectl", "-n", testNamespace, "get", "secret", secret,
			"-o", `jsonpath={.data.token}{"\n"}{.data.ca\.crt}`)
		if err != nil {
			t.Fatalf("reading secret %s: %v", secret, err)
		}

		if code == 0 {
			parts := strings.SplitN(strings.TrimSpace(out), "\n", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				token, tokenErr := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[0]))
				if tokenErr != nil {
					t.Fatalf("decoding the token in secret %s: %v", secret, tokenErr)
				}

				ca, caErr := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
				if caErr != nil {
					t.Fatalf("decoding ca.crt in secret %s: %v", secret, caErr)
				}

				return saCredentials{token: string(token), caPEM: string(ca)}
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("secret %s never carried a token (last output: %q)", secret, out)
		}

		time.Sleep(time.Second)
	}
}

// readyPods lists the pods backing the PostgreSQL Deployment that report
// Ready=True, keyed by name.
func (c *k3sCluster) readyPods(ctx context.Context) (map[string]bool, error) {
	const jsonPath = `jsonpath={range .items[*]}{.metadata.name}{" "}` +
		`{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`

	out, code, err := c.exec(ctx,
		"kubectl", "-n", testNamespace, "get", "pods", "-l", "app="+pgService, "-o", jsonPath)
	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, fmt.Errorf("kubectl get pods exited %d: %s", code, out) //nolint:err113 // test diagnostic
	}

	ready := make(map[string]bool)

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "True" {
			ready[fields[0]] = true
		}
	}

	return ready, nil
}

// waitForReadyPod blocks until a Ready pod backs the Deployment whose name is
// not `excluding`, and returns it.
//
// The exclusion is what makes the pod-restart assertion honest: after deleting
// a pod the old one lingers in Terminating for a moment, and "some pod is
// Ready" would happily match the corpse.
func (c *k3sCluster) waitForReadyPod(ctx context.Context, excluding string, timeout time.Duration) error {
	_, err := c.awaitReadyPod(ctx, excluding, timeout)

	return err
}

func (c *k3sCluster) awaitReadyPod(ctx context.Context, excluding string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)

	var lastErr error

	for {
		ready, err := c.readyPods(ctx)
		if err != nil {
			lastErr = err
		} else {
			for name := range ready {
				if name != excluding {
					return name, nil
				}
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf( //nolint:err113 // test diagnostic
				"no ready pod other than %q for service %s within %s (last error: %v)",
				excluding, pgService, timeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// currentPod returns the name of the (single) Ready pod behind the service.
func (c *k3sCluster) currentPod(ctx context.Context, t *testing.T) string {
	t.Helper()

	name, err := c.awaitReadyPod(ctx, "", 5*time.Minute)
	if err != nil {
		t.Fatalf("resolving the current postgres pod: %v", err)
	}

	return name
}
