---
model: sonnet
effort: medium
---

# Connectivity check: ask about both port-forward verbs, not just `create`

## Goal

Make the `cluster_rbac` stage reflect what the tunnel can actually do. Today
`KubernetesTunnel.PortForwardAllowed` runs a single `SelfSubjectAccessReview`
for `create` on `pods/portforward`, so a Role that grants only `get` — which
admits the **websocket** transport and therefore dials perfectly well — is
reported as `k8s_forbidden`, and a Role that grants only `create` is reported as
fine while every dial through it pays a rejected websocket GET first.

No GitHub issue filed yet — one should be.

## Why

The API server derives the RBAC verb from the HTTP method (`GET` → `get`,
`POST` → `create`, `k8s.io/apiserver/pkg/endpoints/request/requestinfo.go`), and
the two upgrades dbbat attempts use different methods: the websocket upgrade is
a GET (RFC 6455 §4.1), the SPDY fallback a POST. So "may we port-forward here?"
has *two* answers, and the check currently asks only one of them.

`internal/proxy/kubernetes/integration_test.go`
(`TestIntegration_K8s_WebsocketTransport`) pins both behaviours down against a
real k3s cluster, and currently asserts the wrong-but-current answer with a
comment pointing here.

`docs/kubernetes.md` now recommends granting both verbs, which sidesteps the
issue for new deployments but not for existing ones.

## Implementation

- `internal/proxy/upstream/kubernetes.go`: have `PortForwardAllowed` review both
  verbs — one `SelfSubjectAccessReview` each, or keep one call and add a second
  — and report which transports are admitted rather than a bare bool. A small
  struct (`create`, `get`, plus the reason) beats a second bool parameter.
- `internal/proxy/conncheck/kubernetes.go`: fail `cluster_rbac` only when
  *neither* verb is granted. When only one is, succeed but say so in the
  message: `create` alone means every dial takes the SPDY fallback after a
  refused GET; `get` alone means the row cannot work against a pre-1.31 API
  server, which has no websocket handler.
- Update the fake-API-server tests in
  `internal/proxy/upstream/kubernetes_test.go` and
  `internal/proxy/conncheck/kubernetes_test.go`, and flip the
  `assert.False(t, allowed, …)` in the integration suite to whatever the new
  shape says.
