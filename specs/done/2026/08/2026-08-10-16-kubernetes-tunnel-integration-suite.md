---
model: opus
effort: high
---

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

## Implementation Plan

1. **Dependency** — add `github.com/testcontainers/testcontainers-go/modules/k3s`
   (v0.44.0, matching the pinned testcontainers-go).
2. **New package `internal/proxy/kubernetes/`** — an e2e package, not a file in
   `internal/proxy/upstream`, because assertions 2 and 4 need the PostgreSQL
   proxy and `internal/proxy/conncheck`, both of which import `upstream`
   (an in-package test there would be an import cycle). `doc.go` carries no
   build tag so `go build ./...` still sees a package, mirroring
   `internal/proxy/testsupport`.
3. **The cluster is driven with `kubectl` inside the k3s container**, never with
   client-go from the test: `k8s.io/client-go` stays confined to
   `internal/proxy/upstream/kubernetes*.go` (CLAUDE.md, `docs/kubernetes.md`).
   Manifests are copied in and applied; the token Secret and pod state are read
   back with `kubectl get -o jsonpath`.
4. **Cluster row material** — the API server URL and CA bundle come from the
   k3s kubeconfig, parsed *once, in the test* (`gopkg.in/yaml.v3` into the
   module's own `KubeConfigValue`), and the bearer token from the
   `kubernetes.io/service-account-token` Secret, exactly as `docs/kubernetes.md`
   tells an operator to. Nothing in product code ever sees a kubeconfig.
5. **Fixtures** — namespace `data`, a `postgres:15-alpine` Deployment + Service,
   and three ServiceAccounts differing only in their Role:
   - `dbbat` — the documented Role (`create` on `pods/portforward`, `get`/`list`
     on pods and endpointslices);
   - `dbbat-ws` — `get` on `pods/portforward` only;
   - `dbbat-norbac` — no `pods/portforward` verb at all.
6. **Transport assertion, without an audit log.** The apiserver maps the HTTP
   method onto the RBAC verb (`GET` → `get`, `POST` → `create`), and the
   websocket upgrade is a GET while the SPDY upgrade is a POST. So the two
   ServiceAccounts above are a decisive discriminator that needs no
   instrumentation of client-go's opaque fallback dialer: a dial that succeeds
   under `dbbat-ws` can only have gone over the **websocket** transport, and one
   that succeeds under the `create`-only `dbbat` SA can only have gone over the
   **SPDY fallback**. Both are asserted.
7. **The four spec assertions** —
   1. `DialPod` and `Dial("svc/…")` each carry a real pgx session
      (`pgx` with a `DialFunc` that returns the tunnel conn);
   2. a full dbbat PostgreSQL proxy session over the tunnel, asserting the
      queries land in the store's query log;
   3. delete the pod, let the Deployment recreate it, assert the in-flight
      session dies and the *next* `svc/…` connection succeeds;
   4. the no-RBAC ServiceAccount yields `cluster_rbac` / `k8s_forbidden` from
      `conncheck`, on both the cluster row and a database row behind it.
   Plus a TOFU case (spec 2026-08-10-15 landed after this one was written): a
   cluster row with no CA bundle pins the real API server CA on first connect
   and reports `CAPinned`.
8. **Plumbing** — `make test-integration-kubernetes` (`-timeout 40m`), a job in
   `.github/workflows/integration.yml`, and the suite listed in the root
   `CLAUDE.md` and in `docs/kubernetes.md`.
