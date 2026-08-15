# The public demo (`demo.dbbat.com`)

The public demo is a Helm release of `charts/dbbat/` driven by
[`charts/dbbat/values-demo.yaml`](../charts/dbbat/values-demo.yaml). It runs on
the **k8xp** k3s cluster, namespace `dbbat`, and serves two hostnames:
`demo.dbbat.com` and `dbbat.k8xp.com`. The PostgreSQL proxy is published on
`193.70.42.217:5434`.

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
belongs in the chart, not in a hand-applied manifest.

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
```

## Adopting the hand-applied objects (not done yet)

Every object in the namespace predates this values file and was created with a
plain `kubectl apply`, so **the cluster currently holds no Helm release**. A
`helm upgrade --install` against it fails on the first existing object:

```
Unable to continue with install: Service "dbbat-pg" in namespace "dbbat" exists
and cannot be imported into the current release: invalid ownership metadata;
label validation error: missing key "app.kubernetes.io/managed-by" ...
```

Adoption is a live migration and is deliberately left for a separate, attended
change. What it takes:

1. Stamp ownership on each object Helm must take over (`dbbat`, `dbbat-pg`
   services, the three ingresses, the middleware):

   ```bash
   kubectl --context k8xp -n dbbat annotate <kind>/<name> \
     meta.helm.sh/release-name=dbbat meta.helm.sh/release-namespace=dbbat
   kubectl --context k8xp -n dbbat label <kind>/<name> \
     app.kubernetes.io/managed-by=Helm
   ```

2. Delete and let Helm recreate the **Deployment**. Its `spec.selector` is
   immutable and the live one selects `app: dbbat`, where the chart selects the
   `app.kubernetes.io/*` labels — a server-side dry run rejects the change
   outright (`field is immutable`). This is the only step with downtime, a few
   seconds on a single-replica demo.

3. Run the `helm upgrade --install` above and check the pod comes back.

### Deliberate differences from the hand-applied objects

Verified object by object against a `kubectl diff` of the rendered output; these
are the only differences that are not just added Helm labels:

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
- **Port names.** `api`/`pg-proxy` become `http`/`postgres`, and the services
  target them by name. Names only; the numbers are unchanged (8080, 5434).
- **The `dbbat` ClusterIP service no longer exposes 5434.** The chart keeps the
  API service and the proxy service separate, so in-cluster proxy traffic goes
  to `dbbat-pg:5434`. Nothing in the cluster used `dbbat:5434`.
- **NodePort is not pinned.** The live LoadBalancer landed on 31920 by
  allocation, not by request, so the chart does not fix it.
