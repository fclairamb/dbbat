# Adopt the live demo objects into the Helm release

## Goal

Make `demo.dbbat.com` an actual Helm release of `charts/dbbat` with
`charts/dbbat/values-demo.yaml`, so the values file committed by
[2026-08-14-02](../done/2026/08/2026-08-14-02-demo-deployment-manifests-in-repo.md)
is what the cluster runs, not just what it *would* run.

## Why

The values file reproduces every live object and passes a server-side dry run,
but the cluster still holds no Helm release: every object was `kubectl apply`-ed
by hand. Until the adoption happens, the repo and the cluster can drift again —
the exact failure the values file was written to end. Verified with
`helm upgrade --install --dry-run=server`:

```
Unable to continue with install: Service "dbbat-pg" in namespace "dbbat" exists
and cannot be imported into the current release: invalid ownership metadata ...
The Deployment "dbbat" is invalid: spec.selector: ... field is immutable
```

This is a live migration with a few seconds of demo downtime, which is why it
was deliberately left out of that spec.

## Implementation

The procedure is already written up in
[`docs/demo-deployment.md`](../../docs/demo-deployment.md) ("Adopting the
hand-applied objects"). Execute it, attended:

1. Stamp `meta.helm.sh/release-name`, `meta.helm.sh/release-namespace` and
   `app.kubernetes.io/managed-by=Helm` on the two services, the three ingresses
   and the `redirect-https` middleware.
2. Delete the `dbbat` Deployment — its `spec.selector` selects `app: dbbat`
   while the chart selects the `app.kubernetes.io/*` labels, and a selector is
   immutable.
3. `helm upgrade --install dbbat charts/dbbat -f charts/dbbat/values-demo.yaml
   -n dbbat --kube-context k8xp`, then check the pod is Ready, `demo.dbbat.com`
   answers on both :80 (301) and :443, and `psql` still reaches
   `193.70.42.217:5434`.
4. Drop the "not done yet" wording from `docs/demo-deployment.md` once it is,
   leaving the adoption steps as history only if useful.
