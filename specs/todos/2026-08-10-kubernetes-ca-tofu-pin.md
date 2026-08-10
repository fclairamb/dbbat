# Kubernetes tunnel: decide on TOFU-pinning the API server CA

## Goal

Settle whether a `kubernetes` cluster row should learn the API server's
certificate on first connect (TOFU) when no CA bundle was supplied, mirroring
the SSH bastion's `known_host_key` flow — or whether the current "CA bundle
required, with an explicit insecure opt-out" is the right end state.

No GitHub issue filed yet — one should be.

## Why

The originating spec (`specs/done/.../2026-08-10-kubernetes-tunnel.md`) listed
this as optional: *"optionally TOFU-pin the API server CA on first connect when
none is supplied, mirroring the SSH host-key flow"*. It was **not** implemented,
deliberately, and the reasoning deserves recording rather than being silently
inherited:

- SSH TOFU pins the server's *exact* public key. Pinning a CA pins an issuer,
  which still trusts every certificate that issuer signs — a weaker guarantee
  for more machinery.
- A cluster's CA bundle is trivially available at setup time
  (`kubectl get secret <token-secret> -o jsonpath='{.data.ca\.crt}'`), unlike an
  SSH host key, which an operator usually does not have to hand. So the case
  that TOFU exists to rescue barely arises here.
- What shipped instead: `k8s_ca_cert` is **required** on create, with
  `k8s_insecure_skip_tls_verify` as the one explicit, labelled escape hatch for
  throwaway clusters. That is a stronger default than TOFU, not a weaker one.

The open question is whether the *rotation* story changes the calculus: a
cluster whose CA rotates (kind, k3s, some managed control planes) makes a pinned
bundle go stale, and today that surfaces as `cluster_api` with a "certificate
not trusted" message and a manual re-paste.

## Implementation

If the answer is "yes, add it":

- `store.KubernetesServerData` gains a learned-CA field distinct from the
  operator-supplied `CACert` (so "I pasted this" and "we learned this" stay
  distinguishable, and the UI can say which is in force).
- A `SetKubernetesCACert` merge helper alongside `SetKnownHostKey`
  (`internal/store/servers.go`), and a `ServerResolver` method for it.
- `internal/proxy/upstream/kubernetes.go` would need a custom
  `tls.Config.VerifyPeerCertificate` to capture the presented chain on first
  connect, since `rest.Config` has no callback for it — the awkward part.
- The conncheck already reports the TLS-trust failure precisely
  (`isTLSTrustFailure`); it would gain a "pinned on first connect" flag like
  `HostKeyPinned`.

If the answer is "no", record it in `docs/kubernetes.md` so the question is not
re-opened by the next reader of the original spec.
