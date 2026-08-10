# Kubernetes tunnel (port-forward `via` rows)

## Goal

Reach databases running **as pods inside a Kubernetes cluster** the same way SSH
bastions reach databases behind a jump host: a `protocol: kubernetes` server row
acts as a dial path, database targets reference it through `via_uid`, and the
proxy opens a port-forward stream to the pod through the cluster's API server.
No ingress, LoadBalancer, or sshd pod required — access is governed by K8s RBAC
(`pods/portforward`) and shows up in the cluster audit log.

No GitHub issue filed yet — one should be.

## Why

A centrally hosted dbbat cannot reach databases that live only on a cluster's
pod network. Today the workarounds are deploying dbbat in-cluster or running an
sshd relay pod exposed via NodePort/LoadBalancer — both add exposed surface and
per-cluster operational work. Riding the API server needs nothing new exposed,
scopes down to one namespace via a Role, and complements the compliance story
(every tunnel action is in the K8s audit log).

**Scope honesty:** port-forward reaches *a pod's own port*, not arbitrary
`host:port` the way an SSH `direct-tcpip` channel does. A database merely
reachable *from* the cluster network (e.g. RDS in the same VPC) is **out of
scope** — that would need a relay pod. v1 = databases running as pods.

## Implementation

The SSH bastion already carved every groove; this is a second discriminator
value in the same slots.

### Store (`internal/store/models.go`, `servers.go`)

- `ProtocolKubernetes = "kubernetes"`. No migration: the protocol column is
  plain TEXT (see the `ProtocolMSSQL` comment).
- Row reuse: `Host`/`Port` = API server; `PasswordEncrypted` = ServiceAccount
  bearer token (same encryption path as the DB password / SSH key).
- `ServerProtocolData.Kubernetes *KubernetesServerData{ CACert, Namespace }`.
  CA cert is public material stored in clear, like `KnownHostKey`; optionally
  TOFU-pin the API server CA on first connect when none is supplied, mirroring
  the SSH host-key flow.
- `validateViaUID`: accept via rows with protocol `ssh` **or** `kubernetes`
  (cycle checks unchanged). Allow the kubernetes row itself to carry `via_uid`
  → ssh (API server reachable only through a bastion) — falls out of the
  existing recursion since we control the transport dial. Defer ssh-via-k8s.

### Target addressing

When a DB target's via row is kubernetes, its `host` is interpreted as
`<pod-name>` or `svc/<name>` (resolved to a ready pod via EndpointSlice, as
kubectl does), in the tunnel row's namespace; `port` = container port.
Upstream TLS (`ssl_mode`) still applies inside the tunnel, unchanged from SSH.

### Dialer (`internal/proxy/shared/dial.go`)

- Generalize the via dispatch in `sshClientFor` / `DialUpstream`: a kubernetes
  via row yields a pooled port-forward dialer keyed by server UID, like the
  `*ssh.Client` pool, with the same drop-and-retry on stale connections —
  plus re-resolving svc→pod on retry (pods move).
- Use `k8s.io/client-go`'s **lower-level** `httpstream` dialer with the
  websocket→SPDY fallback (`portforward.NewFallbackDialer`), NOT the
  high-level `PortForwarder` (it binds local listeners — wrong shape). One
  stream pair (data + error, `v1.PortHeader`) per DB connection, wrapped in a
  `net.Conn` adapter. Deadlines unsupported is fine — the SSH channel conns
  don't support `SetDeadline` either.
- Isolate client-go in one package (e.g. `internal/proxy/upstream/kubernetes*.go`);
  expect go.sum to grow ~3x.

### Auth — the sharp edge

EKS/GKE kubeconfigs use exec credential plugins; a server daemon cannot run
those. Supported auth = **long-lived ServiceAccount token + CA cert** (mTLS
client certs maybe later). Do not accept kubeconfig uploads. Docs/UI must ship
paste-ready YAML: ServiceAccount + token Secret + namespace-scoped Role
(`create pods/portforward`, `get/list pods`, `get endpointslices`) + binding.

### conncheck (`internal/proxy/conncheck/`)

New classifier: API reachable → token valid → `SelfSubjectAccessReview` on
`pods/portforward` (tells the user the exact missing RBAC verb) → target
pod/service resolvable and ready.

### API / UI / docs

- OpenAPI + server form: "Kubernetes cluster" protocol; fields API URL, CA
  cert, token, namespace. The target form's "via" dropdown lists ssh +
  kubernetes rows (`front/src/routes/_authenticated/servers/`).
- `docs/kubernetes.md`: setup YAML, addressing semantics (`pod` vs `svc/`),
  the reachability-scope limitation, and the stability caveat (port-forward
  rides apiserver→kubelet→runtime; kubelet idle timeout ~4h; pod restarts kill
  streams mid-session).

### Testing

Unit-test the dialer against a fake portforward endpoint (httpstream is
testable in-process). A real-cluster integration suite (kind/k3s via
testcontainers) can follow the existing `//go:build integration` pattern as a
separate target.

---

## Implementation Plan

Layer order, each a commit boundary. Layers 1–3 are load-bearing; 4 is the UI.

### 1. Store (`internal/store/`)

- `models.go`: `ProtocolKubernetes = "kubernetes"` (no migration — protocol is
  plain TEXT). `KubernetesServerData{ CACert, Namespace }` on
  `ServerProtocolData.Kubernetes`. Accessors `KubernetesData()`,
  `IsKubernetes()`, and `IsTunnel()` (= ssh ∨ kubernetes) to replace the
  scattered `IsSSH()`/`protocol <> 'ssh'` checks.
  Row reuse: `Host`/`Port` = API server, `PasswordEncrypted` = ServiceAccount
  bearer token (same AAD-bound encryption as a DB password), `Username` = the
  ServiceAccount name (informational, keeps the required-username shape).
- `servers.go`: `validateViaUID` accepts `ssh` **or** `kubernetes` (cycle checks
  unchanged); a kubernetes row may itself carry `via_uid` → ssh. Every
  "targets only" filter becomes `protocol NOT IN ('ssh','kubernetes')`.
  `ListTunnelServers` returns both kinds for the via dropdown.
  `ServerUpdate` gains `K8sCACert` / `K8sNamespace`, merged into
  `protocol_data.kubernetes` like the Mongo authSource merge.
- `errors.go`: `ErrServerViaNotTunnel` (message mentions ssh *or* kubernetes);
  `ErrServerViaNotSSH` kept as an alias so existing call sites/tests still match.

### 2. Dialer (`internal/proxy/upstream/kubernetes.go` + `shared/dial.go`)

- **All client-go lives in `internal/proxy/upstream`.** `PortForwardDialer`
  built from `{APIServer, CACert, Token, Namespace, Dial}` — the `Dial` hook is
  what lets a kubernetes row hang off an SSH bastion (`rest.Config.Dial`).
- Target addressing: `host` is `<pod>` or `svc/<name>`; `svc/` resolves through
  EndpointSlices (label `kubernetes.io/service-name`) to a **ready** endpoint's
  `targetRef` pod, as kubectl does. Resolution happens on **every** dial, so the
  drop-and-retry path re-resolves for free (pods move).
- Stream setup: `portforward.NewFallbackDialer(websocket, spdy, …)` — the
  **lower-level** `httpstream` dialer, never `PortForwarder` (it binds local
  listeners). One error stream + one data stream per DB connection, keyed by
  `v1.StreamType` / `v1.PortHeader` / `v1.PortForwardRequestIDHeader`. The data
  stream is wrapped in a `net.Conn` adapter; `SetDeadline` returns
  `ErrDeadlineUnsupported`, exactly like the SSH channel conns.
- `shared/dial.go`: `DialUpstream` dispatches on the via row's protocol. A
  second pool (`k8s map[uuid.UUID]*upstream.PortForwardDialer`) mirrors the
  `*ssh.Client` pool, with the same drop-and-retry-once on a stale tunnel.
  `ConnectTunnel` (alongside `ConnectBastion`) validates a kubernetes row alone.
- Auth is **long-lived ServiceAccount token + CA cert only**. No kubeconfig
  upload: EKS/GKE kubeconfigs need exec credential plugins a daemon cannot run.

### 3. conncheck (`internal/proxy/conncheck/kubernetes.go`)

Staged classifier: API server reachable (`cluster_api`) → token valid
(`cluster_auth`) → `SelfSubjectAccessReview` on `pods/portforward`
(`cluster_rbac`, naming the exact missing verb/resource) → target pod or
service resolvable and ready (`cluster_target`). New codes `k8s_forbidden`,
`k8s_target_not_found`, `k8s_target_not_ready`.

### 4. API / UI / docs

- `internal/api/servers.go`: `k8s_ca_cert` / `k8s_namespace` on create+update,
  `k8s_namespace` (+ CA cert, public material) on the response, protocol enum
  and `defaultPortFor` (443) extended, kubernetes rows forced non-listable,
  per-protocol required-field validation (token + namespace).
  `GET /api/v1/tunnel-servers` lists ssh + kubernetes for the via dropdown;
  `/ssh-servers` stays as-is.
- `internal/api/openapi.yml` kept in lockstep (there is a routes↔spec parity
  test), then `bun run generate-client` and commit `front/src/api/schema.ts`.
- Server form: "Kubernetes cluster" protocol with API URL / CA cert / token /
  namespace; via dropdown lists both tunnel kinds and states the pod-only scope.
- `docs/kubernetes.md`: paste-ready ServiceAccount + token Secret +
  namespace-scoped Role (`create pods/portforward`, `get,list pods`,
  `get,list endpointslices`) + RoleBinding; `pod` vs `svc/` addressing; the
  **reachability-scope limitation** (a pod's own port only — an RDS in the same
  VPC is out of scope, that needs a relay pod); and the stability caveat
  (apiserver→kubelet→runtime, ~4h kubelet idle timeout, pod restarts kill
  streams mid-session).

### Testing

Unit tests only, in-process: a fake portforward endpoint built on
`spdy.NewResponseUpgrader` exercises the dialer and the `net.Conn` adapter end
to end; a fake apiserver (`httptest`) covers pod/EndpointSlice resolution and
the conncheck classifier. A real-cluster (kind/k3s) `//go:build integration`
suite is filed as a separate follow-up todo, not done here.
