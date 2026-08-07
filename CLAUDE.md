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
- **API**: `gin-gonic/gin` with OpenAPI 3.0 docs
- **CLI**: `urfave/cli/v3`
- **Config**: `knadh/koanf`
- **Logging**: `log/slog`
- **Frontend**: React 19 + TypeScript + Vite (see `front/CLAUDE.md`)
- **Capture format**: Protocol-agnostic pcapng, readable by tcpdump/Wireshark (`docs/dump-format.md`)
- **Live stream + approvals**: WebSocket event stream and pattern-triggered approval holds (`docs/approvals.md`)

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
│   ├── dump/                # Session packet dump format (read/write/anonymise)
│   ├── api/                 # REST API handlers and middleware
│   │   └── openapi.yml      # OpenAPI 3.0 specification
│   ├── proxy/
│   │   ├── shared/          # Auth, query interception, upstream transport (dial + SSH bastion)
│   │   ├── upstream/        # One upstream-connect path per protocol, shared by the proxies and the connectivity check (ssl_mode policy lives here)
│   │   ├── conncheck/       # Connectivity check: runs the connectors above, classifies the failure
│   │   ├── postgresql/      # PostgreSQL wire protocol proxy
│   │   ├── oracle/          # Oracle TNS/TTC proxy (see docs/oracle.md)
│   │   ├── mysql/           # MySQL/MariaDB proxy (see docs/mysql.md)
│   │   ├── mongodb/         # MongoDB wire protocol proxy (see docs/mongodb.md)
│   │   └── mssql/           # SQL Server TDS proxy (see docs/mssql.md)
│   └── auth/                # OAuth provider abstraction (Slack, etc.)
├── front/                   # React frontend (see front/CLAUDE.md)
├── website/                 # Docusaurus site for dbbat.com
├── docs/                    # Protocol-level technical notes (oracle, mysql, mongodb, mssql, dump format)
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

# Other
make demo             # Build and run in demo mode
make clean            # Clean build artifacts
```

## Development Sessions

**Never kill the running dbbat instance.** It is started beforehand with `make dev` which provides live reload (Air). Restarting it would break the dev workflow. The test mode credentials are `admin`/`admintest`.

## Follow-up Tasks → `specs/todos/`

**Whenever you identify a follow-up task** — an out-of-scope improvement, a deferred fix, a known limitation, or "we should also do X later" — **write it into `specs/todos/` autonomously**, in the same turn, without being asked. Don't just mention it in chat or leave it only in an ephemeral task list: chat scrolls away, `specs/todos/` is the durable backlog.

Conventions (see `specs/README.md`):
- One markdown file per task, named `specs/todos/YYYY-MM-DD-short-kebab-name.md` (date = today).
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
| `DBB_INSTANCE_ID` | Identifies this process among replicas sharing the store (default: hostname). Stamped on connection rows next to a non-configurable per-run UUID, and registered in the `instances` table — keyed by `(instance_id, run_id)` — with a 30s heartbeat. The reconcile closes crash-orphaned connections whose owning *run* deregistered or went 15min without a heartbeat, never a run that is still heartbeating, so replicas sharing an id are safe (just confusing). The liveness half also re-runs every ~7.5min while the process lives, covering both crashed peers and this id's own previous runs. | No |
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
| `DBB_SLACK_NOTIFY_BOT_TOKEN` | Slack bot user OAuth token (`xoxb-...`); empty disables notifications | No |
| `DBB_SLACK_NOTIFY_CHANNEL` | Slack channel id or name for grant-request notifications (default: `#dbbat`) | No |
| `DBB_SLACK_SIGNING_SECRET` | Slack app signing secret; enables Approve/Deny buttons + inbound interactions endpoint. Empty = link-through-UI (no buttons). Requires the bot token. Legacy alias `DBB_SLACK_NOTIFY_SIGNING_SECRET` is also accepted; the canonical name wins if both are set. | No |
| `DBB_SLACK_NOTIFY_APP_TOKEN` | Slack app-level token (`xapp-...`, scope `connections:write`); enables **Socket Mode** — receives Approve/Deny clicks over an outbound WebSocket instead of the inbound endpoint (for deployments Slack can't reach inbound). Requires the bot token. | No |
| `DBB_PUBLIC_URL` | Externally reachable base URL; used for deep-links in Slack notifications | If notify enabled |
| `DBB_APPROVAL_ENABLED` | Enable pattern-triggered approval holds (four-eyes on a statement). **Off by default** — a hold blocks a live database connection on a human. See `docs/approvals.md` | No |
| `DBB_APPROVAL_SLACK_DELAY` | How long a hold stays pending before escalating to Slack (default: `30s`; `0` disables) | No |
| `DBB_APPROVAL_SLACK_SQL` | Include the (truncated) SQL text in the Slack escalation (default: `true`) | No |

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
website's marketing screenshots and the approval-hold video into
`website/static/img/showcase/`. It is not part of `make test-e2e` and never
gates CI.

```bash
make showcase                       # everything
SHOWCASE_PROJECT=video make showcase  # just the clip
SHOWCASE_SKIP_BUILD=1 make showcase   # reuse the existing ./dbbat
SHOWCASE_KEEP=1 make showcase         # leave the stack up to poke at
```

`scripts/showcase.sh` owns the lifecycle: it starts its **own** throwaway
PostgreSQL container and its **own** demo-mode dbbat on ports 8099/5499/5099,
and removes only what it created. It never calls `docker compose` — demo mode
drops every table on startup, so pointing it at the shared dev stack would
destroy that database. Demo-mode credentials are `admin`/`admin`,
`viewer`/`viewer`, `connector`/`connector`.

The approval video drives a real `pg` client through the proxy to create a
genuine hold; the terminal pane and the mouse pointer are drawn (a real
terminal is not reproducible in CI, and Playwright records no cursor). ffmpeg
transcodes the WebM to AV1 (`libsvtav1`) plus an H.264 fallback for Safari.

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
- Optional **approval holds**: RE2 patterns on the definition that suspend a
  matching statement mid-flight until a second human approves it. Self-approval
  is always rejected; a hold has no timeout. Off by default
  (`DBB_APPROVAL_ENABLED`) — see `docs/approvals.md`
- **Definitions are immutably versioned**: an edit archives the current row
  (`archived_at`) and inserts a successor sharing its `lineage_uid`, so a live
  grant's behaviour never changes under it. A slug resolves to the live row.
  **Deactivating** a definition is different from that archival — it withdraws
  the whole lineage and fails closed at auth time; hard deletion is refused
  (409) while anything references it.

### Security
- User passwords: Argon2id hashed
- Database credentials: AES-256-GCM encrypted (AAD-bound to the database UID)
- API keys: encrypted blobs, prefix `dbb_`; cannot create/revoke other keys
- Default admin: `admin`/`admin` (must change on first login)

## API Documentation

- OpenAPI spec: `internal/api/openapi.yml`
- Swagger UI: `GET /api/docs`
- All endpoints versioned under `/api/v1/`
