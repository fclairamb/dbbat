# Put the demo deployment's manifests under version control

## Goal

Make the `demo.dbbat.com` deployment reproducible from this repository instead of
from whatever is currently live in the cluster.

## Why

The public demo runs on the **k8xp** k3s cluster, namespace `dbbat`, and every
object there was applied by hand — `kubectl.kubernetes.io/last-applied-configuration`
shows raw `kubectl apply`, not a Helm release. `charts/dbbat/` exists but is not
what deployed it.

That drift has already cost something. `demo.dbbat.com` was serving **nothing on
port 80** for as long as the ingress has existed (created 2026-01-11): its only
annotation was `traefik.ingress.kubernetes.io/router.entrypoints: websecure`, so
a browser sending plain HTTP for a bare `demo.dbbat.com` — which is what a
browser does — got Traefik's default `404 page not found`. HTTPS worked the whole
time, which is why it went unnoticed: every link on `dbbat.com` is `https://`.
The fix (a `web`-entrypoint ingress plus a `redirectScheme` middleware) was again
applied by hand on 2026-08-14, so it is one `kubectl delete` from being lost with
no record of why it existed.

The memory-limit bump from 512Mi to 1Gi from the same session
([2026-08-14-01](2026-08-14-01-demo-pod-oom-root-cause.md)) is in the same state.

## Implementation

1. Dump the live objects in namespace `dbbat` on the k8xp cluster — deployment,
   both `Ingress` objects plus the new `dbbat-http`, the `redirect-https`
   `Middleware`, services (`dbbat`, the `dbbat-pg` LoadBalancer) — and strip the
   cluster-assigned fields.
2. Decide between two homes and commit one:
   - a `deploy/demo/` directory of plain manifests, applied with
     `kubectl apply -k`, or
   - a values file driving `charts/dbbat/`, which is the reason the chart exists.
   The chart's ingress template already passes annotations straight through, so
   the entrypoint + middleware annotations need no template change; a second
   HTTP-only ingress does.
3. Whichever wins, keep the secret (`dbbat-secret`: DSN, encryption key, demo
   target DB) out of it — reference it, do not template it.
4. Note in `README.md` or `docs/` how the demo is deployed and upgraded, since
   the image tag is currently bumped by hand too (it sits at 0.22.0, built
   2026-08-05).

## Resolved open questions

> Step 2: "Decide between two homes and commit one: a `deploy/demo/` directory
> of plain manifests, applied with `kubectl apply -k`, or a values file driving
> `charts/dbbat/`, which is the reason the chart exists."

**Decision: a values file driving `charts/dbbat/`.** Do not create
`deploy/demo/`. The demo becomes a Helm release of the existing chart, so
`charts/dbbat/` stops being dead code and the demo shares one deployment path
with anything else the chart deploys.

Consequences to carry through the implementation:

- Commit a demo values file (e.g. `charts/dbbat/values-demo.yaml`, or
  `deploy/demo-values.yaml` — pick one and say where in the docs) holding
  everything the live objects carry: image tag, demo run mode, resource
  requests/limits (currently 1Gi, plus whatever
  [2026-08-14-01](2026-08-14-01-demo-pod-oom-root-cause.md) lands for
  `GOMEMLIMIT`), and both ingress definitions.
- The chart's ingress template passes annotations straight through, so the
  `websecure` entrypoint and the `redirectScheme` middleware annotations need
  **no** template change. The second, HTTP-only ingress (`dbbat-http`) **does**
  need a template change — add it to the chart rather than leaving it as a
  stray manifest.
- The Traefik `redirect-https` `Middleware` is a CRD object the chart does not
  currently render; template it in the chart too (guarded so it is only
  rendered when the demo values ask for it), so a single `helm upgrade`
  reproduces the whole demo. Nothing about the demo should require a second
  apply step — the two-step split is exactly the drift this spec exists to end.
- `dbbat-secret` stays out: reference it by name, never template its contents
  (step 3 above, unchanged).
