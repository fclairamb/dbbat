---
model: sonnet
effort: medium
---

# Settings page shows misleading/hard-coded ports; distinguish HTTP vs TCP ports

Originating issue: [#249](https://github.com/fclairamb/dbbat/issues/249)

## Problem
The settings/config page does not reflect the actually-applied listen
configuration. For example it implies the PostgreSQL proxy is on `:5434` when
the running instance is on `:5432`. The frontend has hard-coded defaults
(`front/src/routes/_authenticated/databases/index.tsx:76` — `postgresql:
"5432"`) that can drift from the server's real `DBB_LISTEN_*` values, and the
settings page (`front/src/routes/_authenticated/settings/index.tsx:117`) only
mentions the env vars generically. There is also no distinction between the HTTP
port (behind an HTTP reverse-proxy / ingress) and the TCP proxy ports (behind
TCP load-balancers).

Crucially, there is no way to declare **two different hostnames**: the
**connection host** (e.g. `db.company.com`, reached by direct / TCP
load-balancer access) and the **Web UI host** (e.g. `dbbat.company.com`, reached
through an HTTP ingress / reverse proxy). These are two separate DNS names with
two separate network paths, and the configuration today conflates them.

## Proposal

### Connection host vs Web UI host (core)
dbbat is reached through **two independent network paths** that each need their
own **separately-declarable hostname**:

- **Connection host** — e.g. `db.company.com`. Where SQL clients connect to the
  PG/Oracle/MySQL proxies, via direct / TCP load-balancer access. This already
  exists as the `public.host` global parameter (plus per-protocol
  `public.pg_host` / `public.ora_host` / `public.mysql_host` overrides),
  resolved by `store.ResolvePublicEndpoints`
  (`internal/store/global_parameters.go:207`) and used to build the connection
  strings shown to users (`BuildConnectionURL`).
- **Web UI host** — e.g. `dbbat.company.com`. Where the browser + REST API are
  reached, behind an HTTP ingress / reverse proxy. Today this only exists as the
  `DBB_PUBLIC_URL` env var (`config.PublicURL`, `internal/config/config.go:293`),
  used for Slack deep-links — it is not operator-editable from the UI and is not
  presented as a first-class setting.

Make both hosts first-class, independently declarable settings on the config
page:
- Add the **Web UI host / public base URL** as an editable global parameter
  (persisted alongside the `public.*` params, falling back to `DBB_PUBLIC_URL`
  when unset), so operators can point it at `dbbat.company.com` without an
  env-var redeploy. Use it wherever `config.PublicURL` is consumed today (Slack
  deep-links, and any absolute-URL generation).
- Keep the **connection host** (`public.host` + per-protocol overrides) editable
  as it is today, but label it clearly as the *connection* host, distinct from
  the Web UI host.
- On the config page, present them as two clearly-separated fields with examples
  (`db.company.com` for connections via a TCP load-balancer; `dbbat.company.com`
  for the UI/API via HTTP ingress), so operators understand the two network
  paths and don't confuse them.

### Show effective, non-drifting ports
- Expose the effective runtime listen addresses from the backend (API + Oracle +
  MySQL + PostgreSQL proxies and the API/HTTP port) via an endpoint or the
  existing config/status payload, sourced from the actual resolved config in
  `internal/config` rather than hard-coded frontend constants.
- Update the settings page and the databases page to display the *effective*
  ports from that endpoint instead of hard-coded defaults, so what's shown
  matches what's running.
- Clearly separate, in the UI, the **HTTP port** (API/UI, meant for HTTP reverse
  proxies / ingress) from the **TCP proxy ports** (PG/Oracle/MySQL, meant for
  TCP load-balancers), so operators know which is which when wiring
  infrastructure.

## Implementation Plan

### Prior art already in place (verified, no changes needed)
A 2026-05-14 spec (`specs/done/2026/05/2026-05-14-01-global-parameters.md`)
already built most of the "non-drifting ports" plumbing:
- `GET /instance` (`internal/api/parameters.go:handleGetInstance`) returns
  live `listen.{pg,ora,mysql,api}` addresses straight from `*config.Config`,
  plus `resolved.*` (effective host/port via
  `store.ResolvePublicEndpoints`) and, for admins, the raw `public.*`
  overrides.
- The Settings page's "Local listeners" and "Public advertisement" sections
  (`front/src/routes/_authenticated/settings/index.tsx`) already render from
  `useInstance()` — no hard-coded ports there.
- `front/src/routes/_authenticated/databases/index.tsx:76`'s
  `PROTOCOL_DEFAULT_PORT` map is the conventional port suggested when
  registering a *target* database (5432/1521/3306) — unrelated to dbbat's own
  proxy ports. The per-database "Connection URL" shown in
  `DatabaseDetailsDialog` already comes from `BuildConnectionURL` /
  `ResolvedEndpoints` (`internal/api/connection_url.go`), which is
  live-resolved, not hard-coded. Confirmed via grep: no `5434`/`1522`/`3307`
  literals anywhere under `front/src`. So the spec's problem statement is
  stale on this specific point; nothing to fix in `databases/index.tsx`.

### Genuinely missing, in scope for this pass
1. No first-class, operator-editable "Web UI host" setting — only the
   `DBB_PUBLIC_URL` env var (`config.PublicURL`), consumed solely by Slack
   deep-links, with no UI and no persistence in `global_parameters`.
2. The Settings page doesn't visually distinguish the HTTP (API/UI) listener
   from the three TCP proxy listeners, and doesn't label "Default public
   host" as specifically the *connection* host (as opposed to the Web UI
   host).

### Backend
1. `internal/store/global_parameters.go`:
   - Add `KeyPublicWebUIURL = "web_ui_url"`.
   - Add `WebUIURL string` to `PublicEndpoints` (raw) and `ResolvedEndpoints`
     (effective).
   - `GetPublicEndpoints`/`SetPublicEndpoints`: read/write the new key
     (write-if-non-empty, same convention as the other fields).
   - `ResolvePublicEndpoints`: `WebUIURL` = `pe.WebUIURL` if set, else
     `cfg.PublicURL` (nil-safe), matching the spec's fallback requirement.
   - New `func (s *Store) ResolveWebUIURL(ctx, cfg) string` — best-effort
     (store errors fall back to `cfg.PublicURL`), for callers that just need
     the effective URL as a string (Slack notifier, ephemeral messages).
2. `internal/api/parameters.go`: surface `web_ui_url` in `instancePublicInfo`
   (raw, admin-only) and `instanceResolvedInfo` (effective, all callers);
   accept it in `updateInstancePublicRequest` / `handleUpdateInstancePublic`.
3. `internal/api/server.go`: resolve the Slack notifier's public URL via
   `dataStore.ResolveWebUIURL(ctx, cfg)` instead of raw `cfg.PublicURL`, so a
   DB-stored value takes priority over the env var (still resolved once at
   process start — the notifier itself isn't restructured into a live
   resolver; that's a larger refactor with poor cost/benefit for this pass).
4. `internal/api/slack_interactions.go`: `publicURLForMessage` becomes
   ctx-aware and resolves live via `s.store.ResolveWebUIURL` (nil-store
   guarded) before falling back to `s.config.PublicURL`, then the generic
   placeholder. Update its two call sites.
5. `internal/api/openapi.yml`: add `web_ui_url` to the `PublicEndpoints` and
   `ResolvedEndpoints` schemas with descriptions distinguishing it from the
   connection host; annotate `listen.api` as the HTTP port vs
   `listen.{pg,ora,mysql}` as TCP ports.
6. Tests: extend `internal/store/global_parameters_test.go`
   (`TestPublicEndpoints`, `TestResolvePublicEndpoints`) for `WebUIURL`
   round-trip + `cfg.PublicURL` fallback; extend
   `internal/api/slack_interactions_test.go` if the ctx-aware signature
   needs a new nil-store-fallback case.

### Frontend
7. Regenerate `front/src/api/schema.ts` via `bun run generate-client` after
   the openapi.yml change (part of `make build-front`).
8. `front/src/routes/_authenticated/settings/index.tsx`:
   - Split "Local listeners" into two labeled groups — "HTTP" (`api`) vs
     "TCP proxies" (`pg`/`ora`/`mysql`) — with a one-line blurb on what each
     is for (ingress/reverse-proxy vs TCP load balancer).
   - Add a new "Web UI host" card with the `web_ui_url` field (placeholder
     `dbbat.company.com`), noting the `DBB_PUBLIC_URL` env var fallback and
     that it's used for Slack deep-links / absolute URLs.
   - Relabel the existing public-advertisement card "Connection host"
     (placeholder `db.company.com`) and clarify it's for SQL client
     connections via TCP load balancer / direct access, distinct from the
     Web UI host.
   - Both cards share one form/state and one Save button (the backend PUT
     is atomic across all `public.*` fields).
9. Add `front/e2e/settings.spec.ts`: HTTP/TCP listener grouping is visible;
   admin can set + save the Web UI host field; existing connection-host
   fields still round-trip.

### QA
10. `go build ./...`, `make lint`, `make test`.
11. `make build-front` (includes `generate-client`), `cd front && bun run
    build` (tsc type-check), `cd front && bun run lint`.
12. Run the new Playwright spec if the local devloop is in
    `DBB_RUN_MODE=test`; otherwise author-only and note it in the final
    report.
