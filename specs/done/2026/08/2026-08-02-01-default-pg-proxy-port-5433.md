---
model: sonnet
effort: medium
---

# The default PostgreSQL proxy port should be `5433`

## Problem

The PostgreSQL proxy listens on `:5434` by default
([internal/config/config.go:367](internal/config/config.go:367)). It should be
`5433`.

Two things to know before implementing:

1. **The current default is `5434`, not `5432`.** The request was phrased as
   "5433 instead of 5432", but `5432` has not been the default since
   [specs/done/2026/01/2026-01-10-change-default-port-5434.md](specs/done/2026/01/2026-01-10-change-default-port-5434.md)
   moved it to `5434`. The change to make is `5434` → `5433`.

2. **That earlier spec explicitly considered and rejected `5433`**, on the
   grounds that it is the conventional pgbouncer port and would collide on hosts
   running one. This spec reverses that call: `5433` is the far more recognisable
   "second PostgreSQL" port, and a pgbouncer collision is a one-env-var fix
   (`DBB_LISTEN_PG`). The rejection rationale in the old spec is now stale — do
   not treat it as a blocker, but do mention the pgbouncer caveat in the docs
   where the port is introduced.

This is a user-visible default change: anyone relying on the implicit `:5434`
will need to either repoint their client or set `DBB_LISTEN_PG=:5434`.

## Proposal

Change the default from `:5434` to `:5433` and sweep every place the old value
is hard-coded or documented.

### Code

| File | Change |
|------|--------|
| [internal/config/config.go:367](internal/config/config.go:367) | `ListenPG: ":5434"` → `":5433"` |
| [internal/config/config_test.go:187](internal/config/config_test.go:187) | Expected default `":5434"` → `":5433"` |
| [front/e2e/global-setup.ts:154](front/e2e/global-setup.ts:154) | `DBB_LISTEN_PG: ":5434"` → `":5433"` |
| [docker-compose.yml:36](docker-compose.yml:36), [docker-compose.yml:41](docker-compose.yml:41) | Commented `DBB_LISTEN_PG` and the `5001:5434` mapping → `5433` |

Nothing derives the port at runtime beyond `cfg.ListenPG`
([main.go:338](main.go:338)), and the config page resolves listeners live rather
than hard-coding them (confirmed in
[specs/done/2026/07/2026-07-14-03-config-page-inconsistencies.md](specs/done/2026/07/2026-07-14-03-config-page-inconsistencies.md)),
so no frontend change is expected — verify rather than assume.

### Docs

Replace `5434` with `5433` in:

- [README.md](README.md) — protocol table (l.35), `docker run -p` (l.75), the
  ports line (l.83), the `psql` example (l.186), the env-var table (l.204)
- [CLAUDE.md:142](CLAUDE.md:142) — `DBB_LISTEN_PG` default
- `website/docs/` — `intro.md`, `configuration/index.md`,
  `installation/docker.md`, `installation/docker-compose.md`,
  `installation/binary.md`, `installation/kubernetes.md` (container port,
  service ports, NLB/TCP-route examples, the `5434: dbbat/dbbat:5434` ConfigMap
  entry), `features/supported-databases.md`
- [website/src/pages/index.tsx:105](website/src/pages/index.tsx:105) — the
  landing-page `docker run` snippet

Do **not** rewrite historical specs under `specs/done/` — they record decisions
as they were made.

Beware of false positives when sweeping `5432`: it legitimately appears as the
*target* PostgreSQL port in DSNs, server-registration examples, testcontainers
setup, and the OpenAPI `port` default. Only the proxy's own listen port moves.

### Release note

The commit/PR should flag this as a behaviour change so it lands in the
changelog — e.g. `feat(postgresql)!: default proxy port is now 5433` with a
`BREAKING CHANGE:` body explaining that deployments relying on the implicit
`:5434` must set `DBB_LISTEN_PG=:5434` to keep the old behaviour.

### Verification

- `make test` — config default test passes with the new value.
- `make test-e2e` — the Playwright harness starts the proxy on `:5433`.
- `grep -rn 5434 --include='*.go' --include='*.ts' --include='*.tsx' --include='*.yml' --include='*.md' .`
  returns only `specs/done/` hits.
