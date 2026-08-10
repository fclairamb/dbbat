# Kubernetes tunnel: relay pod (reach the cluster network) and ssh-via-k8s

## Goal

Lift the two scope limits the Kubernetes tunnel shipped with:

1. **Reachability**: today a `kubernetes` via row reaches *a pod's own port* and
   nothing else, so a database merely routable from the cluster network — an RDS
   in the same VPC, a StatefulSet fronted by an ExternalName, a node-local
   service — is out of scope.
2. **Nesting**: an SSH bastion behind a Kubernetes tunnel is refused
   (`shared.ErrKubernetesViaUnsupported`). The reverse — a cluster reached
   through a bastion — is supported.

Both fall out of the same missing piece: a **relay pod**.

No GitHub issue filed yet — one should be.

## Why

The limitation is honest and documented (`docs/kubernetes.md`, the create form,
the `k8s_target_not_found` conncheck message), but it is the first thing an
operator will hit: "our Postgres is RDS, the app runs in the cluster" is at
least as common as "our Postgres is a pod". Right now the answer is "use an SSH
bastion instead", which re-introduces exactly the exposed surface the Kubernetes
tunnel exists to avoid.

It was deliberately deferred rather than skipped: a relay moves the trust
boundary (dbbat would be deploying and trusting a workload inside the customer's
cluster) and adds per-cluster operational work, so it deserves its own design
pass rather than being smuggled into the first version.

## Implementation

Sketch, not a decision — the design choice is the point of this todo.

- **Relay shape.** A tiny operator-deployed pod (a `socat`/`ncat` image, or a
  purpose-built one) that dbbat port-forwards into and then asks to dial
  onward. Two sub-options:
  - *Static*: the operator deploys one relay per namespace and dbbat forwards
    to it; the relay's own config lists what it may reach. Simplest trust story
    — the customer controls the allowlist — and no extra RBAC.
  - *Dynamic*: dbbat creates an ephemeral pod per session. Needs `create pods`
    in the Role, which is a much bigger grant than `create pods/portforward`,
    and makes dbbat responsible for cleanup. Probably not worth it.
  Recommend static.
- **Addressing.** The target row's `host` today is `<pod>` / `svc/<name>`; a
  relayed target needs a third form that says "through the relay, to
  `host:port`". Something like `relay/<host>:<port>`, or a
  `k8s_relay_pod` field on the cluster row plus an ordinary host/port on the
  target — the latter reads better and keeps the target row protocol-shaped.
- **Dialer.** `KubernetesTunnel.Dial` gains a relay branch:
  resolve the relay pod, open the port-forward to *it*, then speak whatever
  handshake the relay uses to request the onward address before handing the
  conn back. Everything downstream (`ssl_mode`, the proxies) is unchanged —
  it is still just a `net.Conn`.
- **ssh-via-k8s** then follows for free: an SSH bastion behind a cluster is
  simply a relayed target whose `host:port` is the bastion. Remove
  `ErrKubernetesViaUnsupported` from `internal/proxy/shared/dial.go` and the
  `sshClientFor` guard, and let `dialViaKubernetes` supply the raw conn that
  `ssh.NewClientConn` handshakes over. `validateViaUID` already accepts the
  chain, so the store needs no change.
- **conncheck.** A relay adds two stages worth distinguishing: "the relay pod
  is not deployed / not Ready" and "the relay refused the onward address".
- **Docs.** `docs/kubernetes.md`'s scope section is written around this limit;
  it becomes the relay's setup section, with its own paste-ready YAML.

## Key files

- `internal/proxy/upstream/kubernetes.go` — `Dial`, `ResolvePod`
- `internal/proxy/shared/dial.go` — `dialViaKubernetes`, `sshClientFor`,
  `ErrKubernetesViaUnsupported`
- `internal/proxy/conncheck/kubernetes.go` — stage/code additions
- `docs/kubernetes.md`, the create form's scope note
  (`front/src/routes/_authenticated/servers/index.tsx`)
