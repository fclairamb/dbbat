# The MSSQL and Kubernetes integration suites still run without the race detector

## Goal

Run `./internal/proxy/mssql/` and `./internal/proxy/kubernetes/` under `-race`,
fix whatever the detector reports, and switch the two remaining `-tags
integration` targets over — `test-integration-mssql` and
`test-integration-kubernetes` in the `Makefile`, and the `mssql` matrix leg
(`race: ""`) plus the `kubernetes` job in `.github/workflows/integration.yml`.

## Why

`2026-08-11-10-race-detector-on-the-integration-suites` turned the detector on
for four of the six integration targets — Oracle, PostgreSQL, MySQL, MongoDB —
and it immediately paid: two live races in the Oracle proxy and one in the
PostgreSQL proxy, all of them the same shape (the client reader and the
upstream reader writing one session's query bookkeeping with no lock between
them). Two of the four suites were clean; one in two was not.

The two left out were left out for want of a run, not for cost. The measured
tax is negligible — the Oracle suite went 6m38s → 6m16s, i.e. inside the noise,
because these suites spend their time booting containers:

| suite | without `-race` | with `-race` |
|-------|-----------------|--------------|
| oracle | 6m38s | 6m16s |
| postgresql | — | 2m37s |
| mysql | — | 2m09s |
| mongodb | — | 2m29s |

- **mssql**: `mcr.microsoft.com/mssql/server` is published for `linux/amd64`
  only, so on an arm64 laptop the suite runs under emulation if it runs at all.
  Nobody has seen what the detector says about the TDS proxy. Given the base
  rate above, assuming it is clean is not a safe default.
- **kubernetes**: a k3s control plane per run makes it the most expensive suite
  to iterate on. It is also the least likely to be hiding this particular bug —
  it exercises the tunnel, not a proxy session's two relay goroutines — but
  "least likely" is not "checked".

## Implementation

- Run each suite once under the detector on a machine that can:
  `go test -race -tags integration -timeout 40m -count=1 -v ./internal/proxy/mssql/`
  (an amd64 host, or a CI dispatch of `integration.yml` with the leg's `race`
  set), and the same for `./internal/proxy/kubernetes/`.
- If it reports races, fix them the way the other two proxies were fixed: one
  per-concern mutex over the session's query bookkeeping, taken once around the
  upstream leg's message switch and in explicit `book()` steps on the client
  leg, with the approval hold outside it. See `trackerMu` in
  `internal/proxy/oracle/session.go` and `bookMu` in
  `internal/proxy/postgresql/session.go` — the second was a near-mechanical
  translation of the first, and a third should be too.
- Then flip the flags and delete the "no `-race` here" comments in the
  `Makefile` and in `.github/workflows/integration.yml` (the `mssql` matrix
  entry's `race: ""` becomes `race: "-race"`).
- Keep `-timeout 40m`. The detector did not come close to needing more on any
  of the four suites already carrying it.

No GitHub issue yet — file one when picking this up.
