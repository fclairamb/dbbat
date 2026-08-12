# DBBat - Database Observability Proxy

A transparent database proxy for query observability, access control, and safety. Supports **PostgreSQL**, **Oracle**, **MySQL/MariaDB**, **MongoDB** and **Microsoft SQL Server**. Every query logged. Every connection tracked.

## Semantic Versioning

This project uses **Conventional Commits** and **release-please** for automated releases.

### PR Title Format

PR titles MUST follow the conventional commit format:
```
<type>(<scope>): <description>
```

**Types:**
| Type | Description | Version Bump |
|------|-------------|--------------|
| `feat` | New feature | Minor (0.x.0) |
| `fix` | Bug fix | Patch (0.0.x) |
| `docs` | Documentation only | None |
| `style` | Code style (formatting, semicolons) | None |
| `refactor` | Code change that neither fixes a bug nor adds a feature | None |
| `perf` | Performance improvement | Patch |
| `test` | Adding or updating tests | None |
| `build` | Build system or external dependencies | None |
| `ci` | CI configuration | None |
| `chore` | Other changes (deps, tooling) | None |

**Scopes** (optional): `api`, `auth`, `config`, `crypto`, `db`, `deps`, `docs`, `dump`, `grants`, `migrations`, `mongodb`, `mssql`, `mysql`, `oracle`, `postgresql`, `proxy`, `store`, `ui`, `release`, `ci`

**Breaking Changes:** Add `!` after type/scope or include `BREAKING CHANGE:` in body for major version bumps.

**Examples:**
- `feat(api): add user export endpoint`
- `fix(proxy): handle connection timeout gracefully`
- `chore(deps): update go dependencies`
- `feat!: redesign authentication flow` (breaking change)

## Technical Stack

- **Language**: Go
- **Storage**: PostgreSQL
- **ORM**: `uptrace/bun` with SQL migrations
- **Proxies**:
  - PostgreSQL wire protocol via `jackc/pgx/v5`
  - Oracle TNS/TTC (hand-rolled) — see `docs/oracle.md`
  - MySQL/MariaDB via `go-mysql-org/go-mysql` (server + client) — see `docs/mysql.md`
  - MongoDB wire protocol (hand-rolled `OP_MSG`; BSON via `go.mongodb.org/mongo-driver/v2`) — see `docs/mongodb.md`
  - Microsoft SQL Server TDS (hand-rolled) — see `docs/mssql.md`
- **Tunnels (`via_uid`)**: a server row with protocol `ssh` or `kubernetes` is a
  *dial path*, never a grantable target. SSH opens a `direct-tcpip` channel to
  any `host:port`; Kubernetes opens a `pods/portforward` stream to **a pod's own
  port** (`host` = `<pod>` or `svc/<name>`, `port` = container port) — a database
  merely routable from the cluster network is out of scope. See
  `docs/kubernetes.md`
- **API**: `gin-gonic/gin` with OpenAPI 3.0 docs
- **CLI**: `urfave/cli/v3`
- **Config**: `knadh/koanf`
- **Logging**: `log/slog`
- **Frontend**: React 19 + TypeScript + Vite (see `front/CLAUDE.md`)
- **Capture format**: Protocol-agnostic pcapng, readable by tcpdump/Wireshark (`docs/dump-format.md`)
- **Live stream + approvals**: WebSocket event stream and pattern-triggered approval holds (`docs/approvals.md`)
- **Tamper-evident audit trail**: `audit_log`, `queries` and `query_rows` are HMAC-chained with a key HKDF-derived from `DBB_KEY`; `dbbat audit verify` walks the chains (`docs/audit-chain.md`)
- **MCP (AI agents)**: Streamable-HTTP endpoint at `/api/v1/mcp` via `github.com/modelcontextprotocol/go-sdk`. Agent statements are executed by dialing dbbat's **own** proxy listener over loopback as the API key's owner — never a parallel internal path. All five protocols (`docs/mcp.md`)

## Project Structure

```
dbbat/
├── main.go                  # Entry point with CLI commands (serve, db, dump)
├── internal/
│   ├── config/              # koanf-based config loading (env, file, CLI)
│   ├── crypto/              # Password hashing (Argon2id) + AES-256-GCM encryption
│   ├── migrations/sql/      # SQL migration files (up/down)
│   ├── store/               # Database models and CRUD operations
│   ├── cache/               # Auth cache shared by API + proxies
│   ├── events/              # In-process topic broker behind GET /api/v1/stream
│   ├── approval/            # Registry of queries parked awaiting a human
│   ├── mcp/                 # MCP server for AI agents; executes SQL by dialing our own proxy listeners (see docs/mcp.md)
│   ├── dump/                # Session packet dump format (read/write/anonymise)
│   ├── api/                 # REST API handlers and middleware
│   │   └── openapi.yml      # OpenAPI 3.0 specification
│   ├── proxy/
│   │   ├── shared/          # Auth, query interception, upstream transport (dial + SSH bastion + K8s tunnel)
│   │   ├── upstream/        # One upstream-connect path per protocol, shared by the proxies and the connectivity check (ssl_mode policy lives here); also the sole home of `k8s.io/client-go` (`kubernetes*.go`)
│   │   ├── conncheck/       # Connectivity check: runs the connectors above, classifies the failure
│   │   ├── postgresql/      # PostgreSQL wire protocol proxy
│   │   ├── oracle/          # Oracle TNS/TTC proxy (see docs/oracle.md)
│   │   ├── mysql/           # MySQL/MariaDB proxy (see docs/mysql.md)
│   │   ├── mongodb/         # MongoDB wire protocol proxy (see docs/mongodb.md)
│   │   ├── mssql/           # SQL Server TDS proxy (see docs/mssql.md)
│   │   └── kubernetes/      # No product code: the `//go:build integration` end-to-end suite for the K8s tunnel (a real k3s cluster) — it lives outside `upstream/` because it also drives the proxy and conncheck, which import it
│   └── auth/                # OAuth provider abstraction
│       ├── slack/           # Slack OIDC login (fixed endpoints, userInfo-based)
│       └── oidc/            # Generic OIDC login: any issuer, verified ID tokens, PKCE
├── front/                   # React frontend (see front/CLAUDE.md)
├── website/                 # Docusaurus site for dbbat.com
├── docs/                    # Protocol-level technical notes (oracle, mysql, mongodb, mssql, kubernetes tunnel, dump format, mcp)
├── docker-compose.yml
└── go.mod
```

## Make Commands

```bash
# Development
make dev              # Start full dev environment (frontend + backend with hot reload)
make dev-front        # Start only frontend dev server
make dev-back         # Start only backend with Air
make dev-stop         # Stop all dev servers

# Building
make build-app        # Build everything (frontend + backend binary)
make build-binary     # Build Go binary only
make build-front      # Build frontend only (to internal/api/resources/)
make build-image      # Build Docker image

# Testing
make test             # Run Go unit tests
make test-e2e         # Run Playwright E2E tests
make lint             # Run golangci-lint

# Website media (on demand only — never part of a release)
make showcase         # Regenerate website/static/img/showcase/ from a live demo instance

# Integration suites (real containers via testcontainers; `make test` skips them)
make test-integration-mongodb      # ./internal/proxy/mongodb/...
make test-integration-mssql        # ./internal/proxy/mssql/...  (amd64 image)
make test-integration-mysql        # ./internal/proxy/mysql/...
make test-integration-postgresql   # ./internal/proxy/postgresql/...
make test-e2e-oracle               # ./internal/proxy/oracle/...
make test-integration-kubernetes   # ./internal/proxy/kubernetes/  (a real k3s cluster)

# Other
make demo             # Build and run in demo mode
make clean            # Clean build artifacts
```

## Development Sessions

**Never kill the running dbbat instance.** It is started beforehand with `make dev` which provides live reload (Air). Restarting it would break the dev workflow. The test mode credentials are `admin`/`admintest`.

## Follow-up Tasks → `specs/todos/`

**Whenever you identify a follow-up task** — an out-of-scope improvement, a deferred fix, a known limitation, or "we should also do X later" — **write it into `specs/todos/` autonomously**, in the same turn, without being asked. Don't just mention it in chat or leave it only in an ephemeral task list: chat scrolls away, `specs/todos/` is the durable backlog.

Conventions (see `specs/README.md`):
- One markdown file per task, named `specs/todos/YYYY-MM-DD-NN-short-kebab-name.md` (date = today).
  `NN` is a two-digit **execution order** within that date, so a plain `ls` of
  `specs/todos/` reads as the queue: lowest date, then lowest number, gets done
  first. Filing a new todo means picking its slot — append it after the last
  `NN` of the day unless it genuinely blocks something already queued, in which
  case renumber. Order by what must happen first (a prerequisite, a suite that
  currently verifies nothing, a live correctness bug), not by when it was written.
- Lead with `# Title`, then `## Goal`, `## Why`, and `## Implementation` (sketch the approach + key files). Enough that someone can pick it up cold.
- Link the originating GitHub issue when there is one: `[#4](https://github.com/fclairamb/dbbat/issues/4)`. If none exists, note that an issue should be filed.
- When a todo is implemented, move its file to `specs/done/YYYY/MM/` (keep the same filename).

This applies even when the current task is otherwise complete — capture the follow-up before moving on.

## CLI Commands

```bash
./dbbat                            # Start server (default command)
./dbbat serve                      # Start server explicitly
./dbbat db migrate                 # Run pending migrations
./dbbat db rollback                # Rollback last migration group
./dbbat db status                  # Show migration status
./dbbat dump anonymise <in> [out]  # Strip session metadata from a .pcapng capture
./dbbat audit verify               # Walk the audit_log HMAC chain; non-zero exit on a break
./dbbat audit verify --queries [--connection <uid>]  # Same for the per-connection query chains
./dbbat audit verify --queries --allow-legacy-stamps  # Tolerate pre-0.24 unkeyed head stamps (removed in 0.25)
./dbbat audit verify --rows [--connection <uid>]     # Same for the per-query captured result row chains
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Yes |
| `DBB_LISTEN_PG` | PostgreSQL proxy listen address (default: `:5433`) | No |
| `DBB_LISTEN_ORA` | Oracle proxy listen address (default: `:1522`; empty disables) | No |
| `DBB_LISTEN_MYSQL` | MySQL/MariaDB proxy listen address (default: `:3307`; empty disables) | No |
| `DBB_LISTEN_MONGO` | MongoDB proxy listen address (default: `:27018`; empty disables) | No |
| `DBB_LISTEN_MSSQL` | SQL Server (TDS) proxy listen address (default: `:1434`; empty disables). 1434/tcp is free — the SQL Server Browser that owns 1434 is UDP-only | No |
| `DBB_LISTEN_API` | REST API listen address (default: `:4200`) | No |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | No |
| `DBB_KEYFILE` | Path to file containing encryption key | No |
| `DBB_RUN_MODE` | Run mode: empty, `test`, or `demo` | No |
| `DBB_LOG_LEVEL` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) | No |
| `DBB_INSTANCE_ID` | Identifies this process among replicas sharing the store (default: hostname). Stamped on connection rows next to a non-configurable per-run UUID, and registered in the `instances` table — keyed by `(instance_id, run_id)` — with a 30s heartbeat. The reconcile closes crash-orphaned connections whose owning *run* deregistered or went 15min without a heartbeat, never a run that is still heartbeating, so replicas sharing an id are safe (just confusing). The liveness half also re-runs every ~7.5min while the process lives, covering both crashed peers and this id's own previous runs. That same tick re-stamps the query-chain head of the sessions **this run** still has open (`Store.RefreshOpenChainStamps`), which is what bounds how long a live session's tail goes unsealed. | No |
| `DBB_DUMP_DIR` | Directory for session dump files (empty = disabled) | No |
| `DBB_DUMP_MAX_SIZE` | Max dump file size per session in bytes (default: 10MB) | No |
| `DBB_DUMP_RETENTION` | Auto-delete dumps older than this (default: `24h`). Applies to the **local spool only** — dbbat never expires objects it uploaded | No |
| `DBB_DUMP_UPLOAD_URL` | Blob bucket finished captures are uploaded to on session close, e.g. `s3://bucket/prefix` (also `file://`, `gs://`, `azblob://` via `gocloud.dev/blob`). Empty = local disk only, the default. Requires `DBB_DUMP_DIR`, which becomes the spool: captures are always written locally and uploaded once complete, never streamed live. Object key `<prefix>/YYYY/MM/DD/<instance-id>/<connection-uid>.pcapng`, recorded on the connection row so downloads never LIST the bucket. Remote retention is the bucket lifecycle policy. See `docs/dump-format.md` | No |
| `DBB_QUERY_STORAGE_RETENTION` | Auto-delete query history (and captured result rows) older than this Go duration. Default `0` = keep forever; `720h` (30 days) is a reasonable opt-in value | No |
| `DBB_MYSQL_TLS_DISABLE` | Refuse TLS upgrade on the MySQL listener (default: `false`) | No |
| `DBB_MYSQL_TLS_CERT_FILE` | PEM cert for MySQL TLS termination (auto self-signed if empty) | No |
| `DBB_MYSQL_TLS_KEY_FILE` | PEM RSA key for MySQL TLS termination (auto-generated if empty) | No |
| `DBB_PG_TLS_DISABLE` | Refuse TLS upgrade on the PostgreSQL listener (default: `false`) | No |
| `DBB_PG_TLS_CERT_FILE` | PEM cert for PostgreSQL TLS termination (auto self-signed if empty) | No |
| `DBB_PG_TLS_KEY_FILE` | PEM key for PostgreSQL TLS termination (auto-generated if empty) | No |
| `DBB_MONGO_TLS_DISABLE` | Keep the MongoDB listener plaintext — refuse TLS termination (default: `false`) | No |
| `DBB_MONGO_TLS_CERT_FILE` | PEM cert for MongoDB TLS termination (auto self-signed if empty) | No |
| `DBB_MONGO_TLS_KEY_FILE` | PEM key for MongoDB TLS termination (auto-generated if empty) | No |
| `DBB_MSSQL_TLS_DISABLE` | Keep the SQL Server listener plaintext — answer `ENCRYPT_NOT_SUP` (default: `false`) | No |
| `DBB_MSSQL_TLS_CERT_FILE` | PEM cert for SQL Server TLS termination (auto self-signed if empty) | No |
| `DBB_MSSQL_TLS_KEY_FILE` | PEM key for SQL Server TLS termination (auto-generated if empty) | No |
| `DBB_MSSQL_TLS_MAX_VERSION` | Client-leg TLS ceiling: `1.2` (default) or `1.3`; anything else fails at startup. TDS encapsulates the handshake and un-wraps it the moment it completes, and at 1.3 the client finishes on a *write*, so drivers differ on whether that last flight is still framed — the proxy sniffs the first byte and follows either choice, but 1.3 is **verified against `go-mssqldb` only** (ODBC/JDBC untested), hence opt-in. The upstream leg stays at 1.2. See `docs/mssql.md` | No |
| `DBB_OIDC_ISSUER` | OIDC issuer URL for the **generic SSO provider** (`internal/auth/oidc`). Setting it enables the provider; discovery is lazy, so an unreachable IdP delays the first login instead of blocking startup. Registered under provider key `oidc`; the callback is `/api/v1/auth/oidc/callback` | No |
| `DBB_OIDC_CLIENT_ID` | OIDC client id. Required once the issuer is set — a half-configured provider fails at startup | If issuer set |
| `DBB_OIDC_CLIENT_SECRET` | OIDC client secret. Same rule | If issuer set |
| `DBB_OIDC_SCOPES` | Space- or comma-separated scopes (default: `openid email profile`); `openid` is always added | No |
| `DBB_OIDC_DISPLAY_NAME` | Login-button label, surfaced through `GET /api/v1/auth/providers` as `display_name` (default: `SSO`) | No |
| `DBB_OIDC_EMAIL_DOMAINS` | Comma-separated allowlist checked against the **verified** ID-token email claim — the generic equivalent of Slack's team-id gating. Empty = any domain the issuer vouches for, which on a multi-tenant issuer (`accounts.google.com`, Entra `/common`) means *any account on the internet* | No |
| `DBB_OIDC_GROUPS_CLAIM` | ID-token claim carrying directory group membership (default: `groups`). Both encodings are accepted — a JSON array and the single-string shorthand — and a lone string is **one** group, never a delimited list. A token that delegates *this* claim through `_claim_names` (Entra's **groups overage**, past ~200 memberships) is read as membership *unknown*, not empty: the login succeeds, `DBB_OIDC_ROLE_MAPPING` is skipped entirely, roles are left untouched with no default-role floor, and a WARN names the user and the claim. dbbat never follows the pointer to Microsoft Graph | No |
| `DBB_OIDC_ROLE_MAPPING` | Binds dbbat roles to directory groups: `admin=db-admins,viewer=analysts`. Pairs split on **commas only** (group names contain spaces); repeat a role to union its groups. Values match exactly, case included — **Entra sends group object ids, not names**. Applied on **every** login, authoritative only for the roles it names, floored at the default role, and refuses to strip the last admin. Empty = roles stay manual. Malformed = startup failure. See `website/docs/configuration/sso.md` | No |
| `DBB_AUTH_AUTO_CREATE_USERS` | Let a verified OAuth/OIDC identity with no local account provision one on first login (default: `true`). Provider-agnostic — it gates Slack, the generic OIDC issuer and anything added later, which is why it is no longer named after Slack. Legacy alias `DBB_SLACK_AUTH_AUTO_CREATE_USERS` is still accepted; the canonical setting wins whenever both are present, **whichever source each came from** (env or config file) — the alias spans two config keys, so precedence is resolved against what the operator actually configured, not just the environment | No |
| `DBB_AUTH_DEFAULT_ROLE` | Role an auto-provisioned account starts with, and the floor `DBB_OIDC_ROLE_MAPPING` never digs below (default: `connector`). Validated against the known roles at startup — a typo is a **startup failure**, not a user silently provisioned into a role nothing recognizes. Matched **exactly**: `Admin` is refused, never folded to `admin`, because the pre-rename code read this value raw and a mis-cased one granted nothing — normalizing it would turn a dormant typo into a real admin default on upgrade alone. Legacy alias `DBB_SLACK_AUTH_DEFAULT_ROLE`, same precedence rule | No |
| `DBB_AUTH_AUTO_CREATE_USERS_<PROVIDER>` / `DBB_AUTH_DEFAULT_ROLE_<PROVIDER>` | Per-provider override of the two settings above (`..._SLACK`, `..._OIDC`); equivalently `auth.providers.<name>.auto_create_users` / `.default_role` in a config file. Each setting resolves **per-provider → instance-wide → default**, which is what lets a deployment trust a tightly-gated Entra tenant to mint accounts while refusing the same from a Slack workspace full of contractors. Two things fail closed at startup rather than becoming an override that quietly does not apply — both would leave the operator with the policy they were trying to change: a per-provider role goes through the same exact-match `KnownRoles` check (`Admin` still refused), and a provider name outside `config.KnownOAuthProviders` is an error. The bool is a `*bool` internally because auto-provisioning defaults to *on*, so "unset" and "false" must be distinguishable. Gating a provider blocks account **creation**, never sign-in by an account that already exists. The legacy `DBB_SLACK_AUTH_*` aliases stay instance-wide — they are not the Slack override | No |
| `DBB_SLACK_NOTIFY_BOT_TOKEN` | Slack bot user OAuth token (`xoxb-...`); empty disables notifications | No |
| `DBB_SLACK_NOTIFY_CHANNEL` | Slack channel id or name for grant-request notifications (default: `#dbbat`) | No |
| `DBB_SLACK_SIGNING_SECRET` | Slack app signing secret; enables Approve/Deny buttons + inbound interactions endpoint. Empty = link-through-UI (no buttons). Requires the bot token. Legacy alias `DBB_SLACK_NOTIFY_SIGNING_SECRET` is also accepted; the canonical name wins if both are set. | No |
| `DBB_SLACK_NOTIFY_APP_TOKEN` | Slack app-level token (`xapp-...`, scope `connections:write`); enables **Socket Mode** — receives Approve/Deny clicks over an outbound WebSocket instead of the inbound endpoint (for deployments Slack can't reach inbound). Requires the bot token. | No |
| `DBB_PUBLIC_URL` | Externally reachable base URL; used for deep-links in Slack notifications | If notify enabled |
| `DBB_APPROVAL_ENABLED` | Enable pattern-triggered approval holds (four-eyes on a statement). **Off by default** — a hold blocks a live database connection on a human. See `docs/approvals.md` | No |
| `DBB_APPROVAL_SLACK_DELAY` | How long a hold stays pending before escalating to Slack (default: `30s`; `0` disables) | No |
| `DBB_APPROVAL_SLACK_SQL` | Include the (truncated) SQL text in the Slack escalation (default: `true`) | No |
| `DBB_MCP_ENABLED` | Serve the Model Context Protocol endpoint at `POST /api/v1/mcp` for AI agents (default: `true`). API-key authenticated; every agent statement runs through the proxy listener over loopback, so grants, quotas, logging and approval holds apply unchanged. All five protocols are covered; on MongoDB the statement is `<command> <extJSON>` rather than SQL. `false` removes the routes entirely. See `docs/mcp.md` | No |

Note: If no encryption key is provided, one is created at `~/.dbbat/key`.

## Testing

### Unit Tests
```bash
make test  # Uses testcontainers-go for PostgreSQL
```

### Protocol Integration Tests

Behind `//go:build integration`, so `make test` neither compiles nor runs them.

```bash
make test-integration-mongodb   # and -mysql, -postgresql, -mssql, plus test-e2e-oracle
```

`make test-integration-kubernetes` is the odd one out: instead of an upstream
database container it boots a **k3s cluster** (privileged container) and runs a
PostgreSQL pod *inside* it, so the tunnel is exercised against a real API server
and a real kubelet — the websocket transport, RBAC as the API server evaluates
it, and a pod that is deleted mid-suite. The cluster is pinned to ≥1.31
(`K3S_TEST_IMAGE` overrides it) because the websocket port-forward handler is
only on by default from there. See `docs/kubernetes.md`.

**Use the Make target, not a bare `go test -tags integration ./internal/proxy/...`.**
Every test starts its own upstream container *and* its own PostgreSQL storage
container, so a suite is dominated by container startup: the MongoDB suite
takes ~4min idle, ~7min with other containers competing for the Docker daemon,
and has been seen past 12min. `go test`'s default timeout is 10min, so the bare
command panics on a busy machine even when nothing is wrong. The targets pass
`-timeout 40m`, matching `.github/workflows/integration.yml`; a run that exceeds
that is a real regression, not a busy laptop.

### E2E Tests
```bash
make test-e2e  # Builds app, starts server in test mode, runs Playwright
```

Test mode credentials: `admin`/`admintest`, `viewer`/`viewer`, `connector`/`connector`

### Website showcase media (`make showcase`)

`front/showcase/` is a **separate Playwright project** that regenerates the
website's marketing screenshots and the two approval-hold clips into
`website/static/img/showcase/`. It is not part of `make test-e2e` and never
gates CI.

```bash
make showcase                       # everything
SHOWCASE_PROJECT=video make showcase  # just the psql clip
SHOWCASE_PROJECT=mcp-poster,mcp-video make showcase  # just the MCP clip + its poster
SHOWCASE_SKIP_BUILD=1 make showcase   # reuse the existing ./dbbat
SHOWCASE_KEEP=1 make showcase         # leave the stack up to poke at
SHOWCASE_PG_TIMEOUT=120 make showcase # give the upstream more time on a cold pull
```

`scripts/showcase.sh` owns the lifecycle: it starts its **own** throwaway
PostgreSQL container and its **own** demo-mode dbbat on ports 8099/5499/5099,
and removes only what it created. It never calls `docker compose` — demo mode
drops every table on startup, so pointing it at the shared dev stack would
destroy that database. Demo-mode credentials are `admin`/`admin`,
`viewer`/`viewer`, `connector`/`connector`.

There are **two** approval clips, the same hold from either side. The first
drives a real `pg` client through the proxy; the second (`mcp-approval-hold-*`)
issues the statement over MCP as an AI agent would — real JSON-RPC against
`POST /api/v1/mcp` with a `dbb_` key — and shows `query` answering
`approval_pending`, the human releasing it, and `await_approval` handing back
the rows. Both draw their client pane and the mouse pointer (neither a real
terminal nor a real agent is reproducible in CI, and Playwright records no
cursor); neither fakes a result. ffmpeg transcodes each WebM to AV1
(`libsvtav1`) plus an H.264 fallback for Safari.

Every run writes `website/static/img/showcase/manifest.json`
(`version`/`commit`/`generatedAt`), version read from
`.release-please-manifest.json` — regeneration is on demand, so the manifest is
what makes staleness visible. `.github/workflows/showcase.yml` exposes the same
pipeline as a `workflow_dispatch` job, plus a non-blocking rot guard that runs
the suite on `front/` pull requests with the output discarded.

The homepage consumes all of it (`website/src/components/ProductShowcase/`),
manifest caption included. It is the site's **only** screenshot set — the old
hand-captured `website/static/img/screenshots/` is gone. The clip never carries
an `autoplay` attribute: playback starts from an effect, and only when the
visitor has not asked for reduced motion.

## Creating Migrations

Add files to `internal/migrations/sql/`:
```
YYYYMMDDHHMMSS_description.up.sql
YYYYMMDDHHMMSS_description.down.sql
```

Use `--bun:split` directive to split multiple statements.

## Core Concepts

### Connection Flow
```
Client → DBBat (auth + grant check) → Target PostgreSQL
Client → DBBat (service-name lookup, O5LOGON proxy auth) → Target Oracle
Client → DBBat (caching_sha2_password / TLS termination) → Target MySQL/MariaDB
Client → DBBat (SCRAM-SHA-256 or PLAIN-over-TLS, authSource lookup) → Target MongoDB
Client → DBBat (TDS PRELOGIN + encapsulated TLS + LOGIN7, SQL auth) → Target SQL Server
```

The same auth + grant + query-logging pipeline runs across all five protocols (`internal/proxy/shared`).

### Access Control
- **Every grant is an instance of a grant definition** and carries no shape of
  its own: `POST /api/v1/grants` assigns a definition to a user + database, and
  a grant request approval materializes the same thing. The grant row holds
  who/where/when, revocation and `priority`; everything below lives on the
  definition and is read through accessors on `store.AccessGrant`.
- Time-windowed grants (`starts_at`, `expires_at`; the length is the
  definition's `duration_seconds`)
- Controls: `read_only`, `block_copy`, `block_ddl` (combinable; empty = full write)
- Optional quotas: `max_query_counts`, `max_bytes_transferred`
- **Two kinds of group, never a bare "group"**: `user_groups` name sets of
  people, `server_groups` name sets of database servers. A definition scopes on
  both — `user_group_uids` (who may request it), `server_group_uids` (which
  databases it covers), `approver_user_group_uids` (who may resolve its holds).
  Empty on either axis means "everyone" / "every database". The pre-rename
  field names `group_uids` / `approver_group_uids` are removed outright and
  are now a 400, same as the per-database `database_uids`.
- **Grants bind to a server group**, not to a database. A grant issued from a
  scoped definition binds to whichever of its groups holds the target database
  and then covers *that group's current membership, and only that*; the anchor
  `database_id` is what an unbound grant covers and what a bound one falls back
  to if its group is deleted (`ON DELETE SET NULL` — narrows, never widens).
  **Membership is live and never snapshotted: adding a server to a group
  immediately widens every live grant bound to it, sessions already running
  included, and removing one narrows them — including the anchor.** That is the single, deliberate exception to the immutable-versioning
  rule below, accepted because group membership is operational data — the admin
  UI warns about the blast radius at the point of edit. One `max_query_counts`
  and one `max_bytes_transferred` budget is consumed across the *whole group*,
  and `priority` ranks group-bound grants against each other on the databases
  where their groups overlap. The auth path is one function,
  `store.GetActiveGrant`, which all five protocols share.
- Optional **approval holds**: RE2 patterns on the definition that suspend a
  matching statement mid-flight until a second human approves it. Self-approval
  is always rejected; a hold has no timeout. Off by default
  (`DBB_APPROVAL_ENABLED`) — see `docs/approvals.md`
- **Definitions are immutably versioned**: an edit archives the current row
  (`archived_at`) and inserts a successor sharing its `lineage_uid`, so a live
  grant's behaviour never changes under it — *except* for the scope it inherits
  from live server-group membership, above. A slug resolves to the live row.
  **Deactivating** a definition is different from that archival — it withdraws
  the whole lineage and fails closed at auth time; hard deletion is refused
  (409) while anything references it.

### Security
- User passwords: Argon2id hashed
- Database credentials: AES-256-GCM encrypted (AAD-bound to the database UID)
- API keys: Argon2id-hashed like passwords (`crypto.HashPassword`), prefix `dbb_`
  with only the first 8 characters kept in clear as `key_prefix` for lookup — a
  leaked store yields no usable key. The one transient exception: a freshly
  minted key sits AES-256-GCM-encrypted in a pending device-authorization row
  (`crypto.DeviceAuthAAD`) until the device polls for it. Keys cannot
  create/revoke other keys
- Default admin: `admin`/`admin` (must change on first login)
- **Roles can follow the directory**: with `DBB_OIDC_ROLE_MAPPING` set, an OIDC
  login resolves the user's roles from the ID token's groups claim on *every*
  sign-in (`internal/api/oauth_roles.go`), so a demotion needs no human. The
  mapping owns only the roles it names — a role granted by hand in the UI is
  never revoked by a login — the default role is the floor, and the last admin
  is retained exactly as `PATCH /users/:uid` refuses to strip it. Every change
  writes a `user.roles_synced` audit entry
- **Tamper-evident audit trail**: every `audit_log` entry, every `queries` row
  and every `query_rows` row carries an HMAC over its content plus the previous
  record's MAC, keyed by an HKDF subkey of `DBB_KEY` that is never stored in the
  database. `audit_log` is one chain; `queries` is one chain **per connection**;
  `query_rows` is one chain **per query**, positioned by `row_number` — an
  ordering, not a dense sequence, because dropped or unencodable rows leave
  gaps. Each scope is chosen so `DBB_QUERY_STORAGE_RETENTION` never severs a
  chain by deleting what it is meant to delete, and each stamps its final head
  when it finishes: the session's on the connection row at close
  (`query_chain_mac`), the capture's on the query row at the flush barrier
  (`row_chain_mac`). A session whose process **crashed** is sealed by the
  reconcile instead, from the stored statements, in the same transaction that
  writes `disconnected_at` — so it seals whatever survived at reconcile time,
  not at the crash. A session that is **still open** is sealed by a third
  writer, `Store.RefreshOpenChainStamps`, which rides the reclaim tick and
  re-stamps the sessions *this run* owns (`run_id = s.runID`, guarded by
  `disconnected_at IS NULL` so it can never overwrite a close's final stamp).
  Its stamp is therefore a **prefix**, not a head, and verification follows:
  while a session is open the stamp is checked against the statement at the
  `chain_seq` it names — a stamp that is merely behind is not a break, one that
  outruns the survivors is — and it goes back to exact the moment the session
  closes. So a live session's tail is protected up to the last sweep (bounded by
  `InstanceReclaimInterval`, not by how long the session lasts); statements
  newer than that sweep are still not. **Both stamps are keyed MACs over the head**, not copies of
  it: a verbatim head is readable out of the table it came from, so copying it
  after a trailing deletion would need no key. `query_chain_mac` additionally
  carries a format version (`query_chain_stamp_version`) *inside* its MAC —
  `0` is the unkeyed stamp 0.23.x wrote, which cannot be re-sealed because the
  key never enters the database; sealing the version is what stops a `1` row
  from being relabelled `0` to get the weaker rule. Since 0.24 a `0` row is a
  **break** ("its tail cannot be verified"), not a weaker pass: tolerating it
  was a standing downgrade path, since replacing a sealed stamp with a raw head
  MAC and version `0` needs no key. `dbbat audit verify --queries
  --allow-legacy-stamps` restores the old counting behavior
  (`sessions_with_legacy_forgeable_head_stamp`) for the single upgrade from
  0.23.x that has such rows, and is removed in 0.25; the REST endpoints have no
  equivalent and no legacy counter. The query walk enumerates **stamped
  connections**, not only connections found in `queries`, so a session whose
  statements are deleted *in full* is judged instead of skipped (deleting all
  but one was always caught). Zero survivors under a keyed stamp is a break
  unless retention can account for it: the store is told its own
  `DBB_QUERY_STORAGE_RETENTION` (`store.Options.QueryRetention`) and excuses
  only a session that **connected before the cutoff**, since every statement
  runs at or after `connected_at` — with retention off, the default, nothing is
  excused, and an excused session is counted as a truncated prefix. Raising or
  disabling retention moves that cutoff backwards, so previously reaped sessions
  can start reading as breaks; lowering it never does. Verify with
  `dbbat audit verify [--queries|--rows]`, or over REST with
  the admin-only `GET /api/v1/audit/verify`, `GET /api/v1/audit/verify/queries`
  and `GET /api/v1/audit/verify/rows` (the last narrows with `?connection=` or
  `?query=`, which cannot be combined) — which
  return counts, the head MAC and the first break, never the key or a record's
  content, and are cached because a walk is O(rows). The endpoint is **not**
  equivalent to the CLI: it is served by the process under audit. See
  `docs/audit-chain.md`

## API Documentation

- OpenAPI spec: `internal/api/openapi.yml`
- Swagger UI: `GET /api/docs`
- All endpoints versioned under `/api/v1/`
