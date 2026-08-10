# Kubernetes tunnel: real-cluster integration suite

## Goal

Add a `//go:build integration` suite that exercises the Kubernetes port-forward
tunnel against a **real** cluster — kind or k3s via testcontainers — behind a
`make test-integration-kubernetes` target, alongside the existing per-protocol
integration suites.

No GitHub issue filed yet — one should be.

## Why

The tunnel shipped with genuinely good in-process coverage: the SPDY upgrade,
the data/error stream pair, the `net.Conn` adapter, svc→pod resolution, the
`SelfSubjectAccessReview` classifier and the whole conncheck ladder are all
tested against a fake API server built on `httpstream`'s own server-side
upgrader (`internal/proxy/upstream/kubernetes_*_test.go`,
`internal/proxy/conncheck/kubernetes_test.go`).

What that cannot cover is everything the *kubelet* does, which is where the
interesting failures live:

- the **websocket-first** transport. The fake refuses the GET so every test
  exercises the SPDY fallback; the tunneled-SPDY-over-websocket path that a
  modern API server actually negotiates has never run.
- RBAC as the API server really evaluates it (the fake just echoes an
  `allowed` boolean back).
- a pod that restarts or is rescheduled mid-session, and whether the
  drop-and-retry re-resolution recovers on the next connection.
- an end-to-end database session: a real PostgreSQL pod, dialed through the
  tunnel, with a grant, query logging and `ssl_mode` applied inside it.

## Implementation

- New file `internal/proxy/upstream/kubernetes_integration_test.go` (and/or a
  `internal/proxy/kubernetes/` e2e package) behind `//go:build integration`,
  matching `internal/proxy/mongodb`'s layout.
- Cluster: `testcontainers-go`'s k3s module (`modules/k3s`) is the cheapest —
  it exposes a kubeconfig, and a ServiceAccount + token Secret + Role can be
  applied with a plain client-go call from the test. kind needs Docker-in-Docker
  and is heavier.
- Derive the dbbat cluster row from the k3s kubeconfig **once, in the test**:
  server URL, CA bundle, and a token minted from the token Secret. This is the
  one place a kubeconfig may be parsed — the product deliberately does not
  accept them (`docs/kubernetes.md`).
- Deploy a `postgres:15-alpine` pod plus a `Service`, then assert:
  1. `DialPod` and `Dial("svc/...")` both reach it;
  2. a full dbbat proxy session over the tunnel logs its queries;
  3. deleting the pod and letting the Deployment recreate it lets the *next*
     connection succeed while the old one dies;
  4. a Role without `pods/portforward` produces `cluster_rbac` /
     `k8s_forbidden` from the conncheck, not a database error.
- `Makefile`: `test-integration-kubernetes` with `-timeout 40m`, mirroring the
  other suites; add it to `.github/workflows/integration.yml`.
