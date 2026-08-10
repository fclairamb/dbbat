# Kubernetes Tunnel — reaching databases that run as pods

A `protocol: kubernetes` server row is a **dial path**, not a database target —
the same kind of thing an `ssh` bastion row is. Point a database row's `via_uid`
at it and dbbat reaches that database by opening a `pods/portforward` stream
through the cluster's API server. No ingress, no LoadBalancer, no sshd relay
pod: nothing new is exposed, access is governed by Kubernetes RBAC, and every
tunnel action lands in the cluster audit log.

## Scope — read this before configuring anything

A port-forward reaches **a pod's own port**, and nothing else.

That is a narrower primitive than an SSH `direct-tcpip` channel, which reaches
any `host:port` the bastion can route to. Concretely:

| Target | Supported |
|---|---|
| PostgreSQL running as a pod in the cluster | ✅ |
| A `Service` in front of such pods (`svc/postgres`) | ✅ |
| RDS / Cloud SQL in the same VPC, routable *from* the cluster | ❌ |
| Anything reachable only via the cluster's node network | ❌ |

The out-of-scope cases would need a relay pod (a socat/sshd sidecar dbbat
port-forwards into, which then dials onward). That is deliberately not part of
v1: it moves the trust boundary and adds per-cluster deployment work, which is
exactly what riding the API server was meant to avoid. If your database is not a
pod, use an SSH bastion row instead.

## Authentication — ServiceAccount token only

dbbat authenticates to the API server with a **long-lived ServiceAccount bearer
token** plus the cluster's CA bundle. That is the whole list.

**dbbat does not accept kubeconfig uploads**, and this is not an oversight. The
kubeconfigs EKS, GKE and AKS generate authenticate through *exec credential
plugins* (`aws eks get-token`, `gke-gcloud-auth-plugin`, `kubelogin`): the
client is expected to fork a binary, with the operator's cloud credentials in
the environment, every time the token expires. A long-running server daemon
cannot do that — there is no operator, no shell, and no credentials to fork
with. Accepting a kubeconfig would therefore work for exactly the clusters that
already use static tokens and fail confusingly for every managed one.

## Cluster setup

Apply this in the namespace your databases run in. It creates a ServiceAccount,
a **non-expiring** token for it, and the minimum Role.

```yaml
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
# Kubernetes 1.24+ no longer auto-creates ServiceAccount token Secrets, and
# projected tokens expire. A dbbat cluster row needs a token that outlives any
# single session, so ask for one explicitly.
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
  # The tunnel itself. This is the verb that matters: without it every dial
  # fails with a 403 out of the stream upgrade.
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
  # Reading pods is what turns "is this pod Ready?" into a useful connectivity
  # check instead of a mysterious hang.
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  # EndpointSlices are how `svc/<name>` resolves to a ready pod, the same way
  # `kubectl port-forward svc/...` does it.
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
```

Then read out the two values dbbat needs:

```bash
# The bearer token — this is a credential; treat it like a password.
kubectl -n data get secret dbbat-token -o jsonpath='{.data.token}' | base64 -d

# The API server's CA bundle.
kubectl -n data get secret dbbat-token -o jsonpath='{.data.ca\.crt}' | base64 -d

# The API server address.
kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'
```

The Role is **namespace-scoped on purpose**: a ClusterRole would let dbbat
port-forward into any pod in the cluster, and one namespace per cluster row is
the granularity the model is built around.

## The dbbat rows

**The cluster row** (`protocol: kubernetes`):

| Field | Value |
|---|---|
| `host` / `port` | the API server, e.g. `https://api.cluster.example.com` and `6443` |
| `username` | the ServiceAccount name — informational, shown in the UI |
| `password` | the ServiceAccount bearer token (encrypted at rest, never returned) |
| `k8s_ca_cert` | the PEM CA bundle (public; round-trips in the API). **Required**, unless the escape hatch below is set |
| `k8s_namespace` | the namespace the Role covers |
| `k8s_insecure_skip_tls_verify` | off by default; see [Skipping TLS verification](#skipping-tls-verification) before turning it on |
| `via_uid` | *optional* — an SSH bastion row, when the API server is itself only reachable through a jump host |

A cluster row is never listable and never grantable; like an SSH bastion it
exists only to be referenced.

**The database row** — an ordinary `postgresql` / `mysql` / … row whose
`via_uid` points at the cluster:

| Field | Value |
|---|---|
| `host` | `<pod-name>`, or `svc/<service-name>`, **in the cluster row's namespace** |
| `port` | the **container** port (5432, 3306, …) — not a NodePort, not a Service port mapping |
| `ssl_mode` | applies unchanged, *inside* the tunnel |

`svc/<name>` is resolved through the service's EndpointSlices to a **ready**
endpoint, re-resolved on every connection — so a pod that gets rescheduled does
not require touching the row. A bare name (or `pod/<name>`) addresses one pod
directly and costs no API call.

Note that `port` is the container port because a port-forward terminates in the
kubelet and speaks to the pod directly; a `Service`'s port *mapping* is never
applied. `svc/postgres` with port 5432 means "a ready pod behind the service
`postgres`, port 5432 on that pod".

### Skipping TLS verification

`k8s_insecure_skip_tls_verify` disables verification of the API server's
certificate. It is the only alternative to pinning a CA bundle, and it is off by
default.

**What it costs.** The API server connection is the one carrying the
ServiceAccount bearer token, on every request. With verification off, anything
positioned to intercept that connection — a compromised egress proxy, a
misdirected DNS answer, someone on a shared network — can present its own
certificate, terminate the connection, and **read the token**. That token is
enough to port-forward into every pod your Role covers. It is not a "slightly
weaker TLS" setting; it is "the credential is now interceptable".

**When it is defensible.** A throwaway cluster whose CA rotates faster than
anyone will re-paste it: a local kind/k3s instance, an ephemeral CI cluster, a
demo. In other words, when the token itself is worthless.

**When it is not.** Anything holding real data. If the difficulty is that you
cannot find the CA bundle, it is in the token Secret you already created:

```bash
kubectl -n data get secret dbbat-token -o jsonpath='{.data.ca\.crt}' | base64 -d
```

**What dbbat does about it.** Two things, deliberately:

- A row that pins **no** CA bundle and has **not** set this flag is refused —
  by the API on create *and* update, and again by the dialer itself. There is
  no silent third state where dbbat falls back to the host's system trust
  store, because "no CA configured" must never quietly mean "trust any
  publicly trusted certificate for that hostname".
- Rows with the flag set are labelled as such in the servers list and in the
  edit dialog, so an insecure row cannot hide once it exists.

## Connectivity check

`POST /api/v1/servers/{uid}/test` (or the "Test" button) walks the cluster row
in the order you would debug it, and the stage it stops at is the answer:

| Stage | Question |
|---|---|
| `config` | is the row usable at all? — including the refusal above, when it pins no CA and has not opted out |
| `cluster_api` | can we reach the API server? (DNS, routing, firewall, and whether the pasted CA bundle trusts it) |
| `cluster_auth` | does the API server accept the token? |
| `cluster_rbac` | a `SelfSubjectAccessReview` on `pods/portforward` — may this ServiceAccount actually open a tunnel here? |
| `cluster_target` | does the pod (or the ready pod behind `svc/<name>`) resolve, and is it Ready? |

The RBAC question is asked explicitly rather than inferred from a failed
port-forward, because that is the difference between being told to add `create`
on `pods/portforward` to the Role and reading a 403 out of a stream-upgrade
error. Testing a *database* row behind a cluster runs the same pre-flight before
it dials, so a missing verb is never reported as the database refusing you.

## Stability caveats

Port-forwarding is a chain — apiserver → kubelet → container runtime — and it
was designed for interactive `kubectl` sessions, not for long-lived server
connections. What that means in practice:

- **The kubelet drops idle streams**, by default after about 4 hours
  (`--streaming-connection-idle-timeout`). A database session held open and
  silent past that will be cut.
- **A pod restart kills every stream into it**, mid-transaction. There is no
  reconnect that preserves session state; the client sees the connection drop.
- **Rescheduling moves the pod.** Existing sessions die with it; new ones
  re-resolve, so `svc/<name>` recovers on the next connection while a bare pod
  name does not.
- **The API server is now on the data path** for every byte. A control plane
  under load, or rate-limiting the ServiceAccount, shows up as database latency.

None of this makes the feature unsuitable — it makes it unsuitable for treating
a pod database as if it had a stable network identity. Size connection pools and
retry policies accordingly.

## Transport details

dbbat uses client-go's **low-level `httpstream` dialer**, not the high-level
`PortForwarder`: the latter binds local TCP listeners and copies between them,
which is the wrong shape when what you need is a `net.Conn`. Each database
session creates one error stream plus one data stream (`v1.PortHeader`,
`v1.PortForwardRequestIDHeader`) over its own upgraded connection, and the data
stream is wrapped as a `net.Conn`.

The upgrade is attempted **websocket-first with a SPDY fallback**, matching
kubectl: SPDY is deprecated but remains the only transport older API servers
speak. The one exception is a cluster row that sits behind an SSH bastion — then
the SPDY transport is used unconditionally, because client-go's websocket round
tripper exposes no dial hook and would ignore the bastion entirely.

`SetDeadline` on the resulting conn returns `os.ErrNoDeadline`. This matches the
SSH bastion path, whose channel conns reject deadlines the same way, so every
caller that already copes with a tunneled connection copes with this one.

The error stream matters more than it looks: a port-forward to a port nothing
listens on **upgrades successfully** and only then reports the failure there. It
is drained for the life of the connection and its message is surfaced in place
of the otherwise unexplained EOF.

All of `k8s.io/client-go` is confined to `internal/proxy/upstream/kubernetes*.go`
so the dependency tree it drags in touches exactly one package.

## Nesting

- **A cluster behind an SSH bastion**: supported. Set the cluster row's
  `via_uid` to the bastion; every connection to the API server is dialed
  through it.
- **An SSH bastion behind a cluster**: refused. The store lets the chain
  validate, but the dialer rejects it explicitly rather than mis-dialing —
  reaching an sshd that is not itself a pod would need the relay pod this
  version does not have.

## Related

- `docs/postgresql.md`, `docs/mysql.md`, … — the per-protocol proxies, which are
  unchanged by the tunnel: the same auth, grant, logging and approval pipeline
  runs whether the upstream conn came from a TCP dial, an SSH channel, or a
  port-forward stream.
