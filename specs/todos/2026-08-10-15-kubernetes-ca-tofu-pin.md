---
model: opus
effort: high
---

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
- What shipped instead: `k8s_ca_cert` is **required** — on create, on update,
  and again in the dialer, which refuses a row that pins no CA rather than
  falling back to the host's system trust store — with
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

## Resolved open questions

> Settle whether a `kubernetes` cluster row should learn the API server's
> certificate on first connect (TOFU) when no CA bundle was supplied … or
> whether the current "CA bundle required, with an explicit insecure opt-out"
> is the right end state.

**Decision: yes — implement TOFU.** Follow the spec's "If the answer is 'yes,
add it'" section in full. The rotation story is what settles it: a pinned
bundle going stale currently costs a manual re-paste, and TOFU plus an explicit
re-pin is a better operator experience than requiring the bundle up front.

This changes a shipped constraint, so the semantics have to be precise:

- **`k8s_ca_cert` becomes optional**, on create and on update. Removing that
  requirement is the actual behaviour change — without it there is no "no CA
  supplied" case for TOFU to serve. Update the create/update validation and the
  create form's copy accordingly.
- **An operator-supplied CA always wins.** When `CACert` is set, verify against
  it and never TOFU. TOFU applies only when it is empty.
- **Learned is stored separately.** `store.KubernetesServerData` gains a
  learned-CA field distinct from `CACert`, so "I pasted this" and "we learned
  this" stay distinguishable and the UI can say which is in force. Add
  `SetKubernetesCACert` alongside `SetKnownHostKey`
  (`internal/store/servers.go`) plus the `ServerResolver` method.
- **Never fall back to system trust.** With neither a supplied nor a learned CA
  and `k8s_insecure_skip_tls_verify` false, the dialer still refuses — exactly
  as it does today. TOFU adds a *first* connect that learns; it does not add a
  connect that trusts the host's store.
- **A mismatch after pinning is a hard failure**, mirroring SSH's changed
  host key: refuse, and report it distinctly from "certificate not trusted"
  so an operator can tell rotation from interception. Give the conncheck a
  "pinned on first connect" flag like `HostKeyPinned`, and provide the way to
  clear a stale pin (re-paste a bundle, or explicitly reset the learned value)
  — rotation is the case that motivated saying yes, so it must have an exit.
- `internal/proxy/upstream/kubernetes.go` needs a custom
  `tls.Config.VerifyPeerCertificate` to capture the presented chain, since
  `rest.Config` exposes no callback. The spec flags this as the awkward part;
  pin the capture with a unit test against the existing fake API server.

**Docs.** `docs/kubernetes.md` gains the TOFU flow and, importantly, keeps the
reasoning already recorded here — that pinning a CA pins an *issuer* and is a
weaker guarantee than SSH's exact-key pin, which is why a supplied bundle stays
the recommended setup and TOFU is the fallback, not the default advice.
