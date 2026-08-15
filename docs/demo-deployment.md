# The public demo (`demo.dbbat.com`)

The public demo is a Helm release of `charts/dbbat/` driven by
[`charts/dbbat/values-demo.yaml`](../charts/dbbat/values-demo.yaml). It runs on
the **k8xp** k3s cluster, namespace `dbbat`, and serves two hostnames:
`demo.dbbat.com` and `dbbat.k8xp.com`. The PostgreSQL proxy is published on
`193.70.42.217:5434`.

The release is named `dbbat` in namespace `dbbat` and is real: `helm list -n
dbbat` shows it. Every object in the namespace belongs to it. Nothing there is
hand-applied any more — the previously `kubectl apply`-ed objects were adopted
into the release on **2026-08-15** (see
[History: the adoption](#history-the-adoption-2026-08-15)), so `helm upgrade` is
the only deployment path.

It runs with `DBB_RUN_MODE=demo`, which **drops every table on startup** and
reseeds the sample data. Never point these values at a store holding anything
real.

## Deploy / upgrade

```bash
helm upgrade --install dbbat charts/dbbat \
  -f charts/dbbat/values-demo.yaml \
  -n dbbat --kube-context k8xp
```

That single command reproduces the whole demo — deployment, both services, all
three ingresses and the Traefik middleware. There is no second `kubectl apply`
step; if something about the demo cannot be expressed in the values file, it
belongs in the chart, not in a hand-applied manifest. Follow it with the
[post-upgrade smoke test](#post-upgrade-smoke-test).

### Bumping the version

The demo does **not** follow releases automatically. The image tag is bumped by
hand, in `charts/dbbat/values-demo.yaml`:

```yaml
image:
  tag: "0.22.0"   # <- edit, commit, then run the helm upgrade above
```

It currently sits at **0.22.0** (built 2026-08-05). Commit the bump before
running the upgrade, so the repository and the cluster never disagree about
what is deployed.

### If you change the memory limit

`resources.limits.memory` and `goMemLimit` must move together. The demo runs a
**1Gi** limit with **`GOMEMLIMIT=768MiB`**; the chart's own default is `192MiB`,
sized for the chart's default 256Mi limit, so dropping the explicit value from
the demo values would quietly cut the demo's ceiling by four. Helm cannot do
arithmetic on a quantity like `1Gi`, so this is a manual pairing. Background:
[`specs/done/2026/08/2026-08-14-01-demo-pod-oom-root-cause.md`](../specs/done/2026/08/2026-08-14-01-demo-pod-oom-root-cause.md).

## The secret is **not** in this repository

The release expects an existing secret named `dbbat-secret` in namespace
`dbbat`, referenced by name and never templated:

| Key | Contents |
|-----|----------|
| `dsn` | PostgreSQL DSN of dbbat's own store (`DBB_DSN`) |
| `encryption-key` | base64 AES-256 key (`DBB_KEY`) |
| `demo-target-db` | DSN of the sample upstream the demo exposes (`DBB_DEMO_TARGET_DB`) |

Recreating it from scratch (values come from the password store, never from
this repo):

```bash
kubectl --context k8xp -n dbbat create secret generic dbbat-secret \
  --from-literal=dsn='postgres://...' \
  --from-literal=encryption-key="$(openssl rand -base64 32)" \
  --from-literal=demo-target-db='postgres://...'
```

Replacing `encryption-key` invalidates every stored database credential — demo
mode reseeds them, but nothing else would.

## Verifying a change before deploying

```bash
# 1. Schema + template sanity
helm lint charts/dbbat -f charts/dbbat/values-demo.yaml

# 2. What would be rendered
helm template dbbat charts/dbbat -f charts/dbbat/values-demo.yaml -n dbbat

# 3. What the cluster would actually change (server-side, read-only)
helm template dbbat charts/dbbat -f charts/dbbat/values-demo.yaml -n dbbat \
  | kubectl --context k8xp -n dbbat diff -f -

# 4. What Helm itself would do to the release (server-side dry run)
helm upgrade --install dbbat charts/dbbat -f charts/dbbat/values-demo.yaml \
  -n dbbat --kube-context k8xp --dry-run=server
```

Steps 3 and 4 both work now that the namespace is a real release; before the
adoption they aborted on the Deployment's immutable `spec.selector`.

## Post-upgrade smoke test

Run this after every `helm upgrade`. It is the checklist the adoption was
signed off against, and it catches the failure described in the next section —
which a pod-Ready check alone does **not**.

```bash
# Release itself
helm list -n dbbat --kube-context k8xp                 # dbbat, STATUS deployed

# Pod: Ready, 0 restarts, expected image
kubectl --context k8xp -n dbbat get pods -o wide
kubectl --context k8xp -n dbbat get deploy dbbat \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Memory pairing actually landed (see "If you change the memory limit")
kubectl --context k8xp -n dbbat get cm dbbat -o jsonpath='{.data.GOMEMLIMIT}{"\n"}'
kubectl --context k8xp -n dbbat get deploy dbbat \
  -o jsonpath='{.spec.template.spec.containers[0].resources}{"\n"}'

# Endpoints — the one check that catches a broken selector
kubectl --context k8xp -n dbbat get endpoints
#   dbbat     ...:8080,...:5434
#   dbbat-pg  ...:5434
# Either of them showing <none> means no pod matches the Service selector.

# Reachability
curl -sS -o /dev/null -w '%{http_code}\n' http://demo.dbbat.com    # 301 -> https
curl -sS -o /dev/null -w '%{http_code}\n' https://demo.dbbat.com   # 302
curl -sS -o /dev/null -w '%{http_code}\n' https://dbbat.k8xp.com   # 302
nc -z -w 5 193.70.42.217 5434 && echo "proxy port open"
```

Reference values from the 2026-08-15 upgrade: image
`ghcr.io/fclairamb/dbbat:0.22.0`, `GOMEMLIMIT=768MiB`, limits
`{cpu: 500m, memory: 1Gi}`, requests `{cpu: 50m, memory: 128Mi}`.

## Gotcha: adopting an object leaves stale `spec.selector` keys behind

**Read this before adopting any other hand-applied object into a chart.** It is
what actually broke the demo during the 2026-08-15 adoption, and it is a nasty
one to debug cold because every obvious signal looks healthy.

**Symptom.** `helm upgrade` reports success, the release is `deployed`, the pod
is **Ready with zero restarts** and its logs are clean — and yet
`demo.dbbat.com` returns **503** and the proxy port on `193.70.42.217:5434` is
**closed**. `kubectl -n dbbat get endpoints` shows both `dbbat` and `dbbat-pg`
with **no endpoints at all**.

**Cause.** Helm patches an adopted object with a strategic merge, and
`spec.selector` on a Service is a plain map: a merge patch *adds* keys, it never
removes one the chart no longer sets. The hand-applied Services selected
`app: dbbat`; the chart selects the two `app.kubernetes.io/*` keys. The merge
kept all three:

```json
{"app":"dbbat","app.kubernetes.io/instance":"dbbat","app.kubernetes.io/name":"dbbat"}
```

A Service selector is ANDed, and the pod Helm now owns carries only the
`app.kubernetes.io/*` labels — no `app: dbbat` — so **neither Service matched
any pod**. There was no useful `last-applied-configuration` to drive a removal
either, because the objects were created by raw `kubectl apply` outside Helm.

**Fix.** Delete the stale key explicitly with a JSON-merge `null`, once per
Service:

```bash
for svc in dbbat dbbat-pg; do
  kubectl --context k8xp -n dbbat patch svc "$svc" \
    -p '{"spec":{"selector":{"app":null}}}'
done
```

Endpoints populate immediately and the service recovers. Afterwards the
selectors are exactly what the chart renders, so subsequent upgrades are clean.

This is purely an artefact of **adoption**. A fresh `helm install` into an empty
namespace has no stale key and never hits it, and the chart needs no change.

## History: the adoption (2026-08-15)

Kept as the record of why the objects carry Helm ownership metadata. **This is
done** — do not re-run it.

Before it, every object in the namespace had been created with a plain `kubectl
apply` and the cluster held no Helm release at all, so `helm upgrade --install`
failed on the first existing object:

```
Unable to continue with install: Service "dbbat-pg" in namespace "dbbat" exists
and cannot be imported into the current release: invalid ownership metadata;
label validation error: missing key "app.kubernetes.io/managed-by" ...
```

The attended migration, run on 2026-08-15 with **12 seconds** of demo downtime:

1. Stamp ownership on each object Helm had to take over — the `dbbat` and
   `dbbat-pg` services, the `dbbat`, `dbbat-http` and `dbbat-k8xp` ingresses,
   and the `redirect-https` Traefik middleware:

   ```bash
   kubectl --context k8xp -n dbbat annotate <kind>/<name> \
     meta.helm.sh/release-name=dbbat meta.helm.sh/release-namespace=dbbat
   kubectl --context k8xp -n dbbat label <kind>/<name> \
     app.kubernetes.io/managed-by=Helm
   ```

2. Delete the **Deployment** and let Helm recreate it. Its `spec.selector` is
   immutable and the live one selected `app: dbbat`, where the chart selects the
   `app.kubernetes.io/*` labels — a server-side dry run rejected the change
   outright (`field is immutable`). This was the only step with downtime.

3. `helm upgrade --install dbbat charts/dbbat -f charts/dbbat/values-demo.yaml
   -n dbbat --kube-context k8xp --wait`, producing release `dbbat` revision 1.

4. Clear the stale Service selector keys the merge left behind — see the gotcha
   above. Without this the demo stays down at 503 despite a Ready pod.

### Deliberate differences from the previously hand-applied objects

What the adoption deliberately changed. Verified object by object against a
`kubectl diff` of the rendered output beforehand; these were the only
differences that were not just added Helm labels:

- **Pod hardening.** The chart runs the container as non-root (65532) with a
  read-only root filesystem, dropped capabilities and a `RuntimeDefault`
  seccomp profile, plus a dedicated ServiceAccount with no mounted token. The
  hand-applied deployment had none of that. The image is distroless `:nonroot`,
  so it already ran as 65532.
- **Config through a ConfigMap.** `DBB_RUN_MODE`, `DBB_LISTEN_PG`,
  `DBB_LISTEN_API` and `GOMEMLIMIT` move from inline `env` into the chart's
  ConfigMap (same values). `DBB_BASE_URL: /app` is added, which is the
  application's own default.
- **Probe timings** follow the chart's defaults (readiness every 5s, liveness
  every 10s) rather than the hand-applied 10s/30s.
- **Service `targetPort`s are named, not numeric.** The old services targeted
  8080/5434 by number; the rendered ones target the container ports `http` and
  `postgres`, which are those same numbers. The service port *names* (`api`,
  `pg-proxy`) and numbers are unchanged — `service.api.portName` and
  `service.proxy.portName` keep them matching what was there, and
  `service.api.includeProxyPort` keeps 5434 on the `dbbat` ClusterIP service
  next to 8080, as the hand-applied object had it.
- **NodePort is not pinned.** The LoadBalancer had landed on 31920 by
  allocation, not by request, so the chart does not fix it.

Note that `spec.selector` is *not* in this list, and could not be: a merge patch
cannot drop a key. That is the gotcha above, and it had to be fixed by hand.
