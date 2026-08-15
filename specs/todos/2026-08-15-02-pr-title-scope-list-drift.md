# Reconcile the PR-title scope list with CLAUDE.md

## Goal

Make the scopes `CLAUDE.md` documents and the scopes
`.github/workflows/semantic-pr.yml` accepts the same list, so a PR titled with a
documented scope cannot fail CI.

## Why

They disagree today, and the disagreement is only discoverable by opening a PR
and watching `Validate PR title` go red.

`CLAUDE.md`'s **Scopes** line lists:

```
api, auth, config, crypto, db, deps, docs, dump, grants, migrations,
mongodb, mssql, mysql, oracle, postgresql, proxy, store, ui, release, ci
```

`amannn/action-semantic-pull-request` in `.github/workflows/semantic-pr.yml`
accepts:

```
api, auth, config, crypto, db, deps, docs, dump, grants, migrations,
mysql, oracle, postgresql, proxy, store, ui, release, ci
```

So **`mongodb` and `mssql` are documented but rejected** — the two protocols
dbbat added most recently, and the two whose scopes a contributor is therefore
most likely to reach for. `dbbat` supports five protocols and three of them have
a usable PR scope.

Separately, `deploy` is used freely in *commit* subjects on `main`
(`feat(deploy): put the demo.dbbat.com deployment in a values file`,
`fix(deploy): keep the demo's proxy port on the dbbat ClusterIP service`) but is
in neither list, so it works in a commit and fails in a PR title. This was hit
for real on 2026-08-15: PR #322 (`chore(deploy): move the demo to v0.25.0`) was
rejected with `Unknown scope "deploy"` and had to be retitled.

The failure is cheap to fix once you know, but it costs a CI round-trip and it
punishes exactly the contributor who bothered to read `CLAUDE.md`.

## Implementation

1. Decide the canonical list. Suggested: take the union, i.e. add `mongodb`,
   `mssql` and `deploy` to the workflow. `charts/` and the demo values are real,
   long-lived surfaces, so `deploy` earns a scope rather than being spelled
   `chore:` with no scope.
2. Update the `scopes:` input in `.github/workflows/semantic-pr.yml`.
3. Update the **Scopes** line in `CLAUDE.md` to match exactly.
4. Consider whether the two can be driven from one source rather than kept in
   sync by hand — a comment in each pointing at the other is the cheap version,
   and is probably enough given how rarely the list changes.

Note that `requireScope: false`, so dropping the scope entirely is always a
valid escape hatch; this is about not making people discover that the hard way.
