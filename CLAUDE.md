# DBBat - Database Observability Proxy

A transparent database proxy for query observability, access control, and safety. Supports **PostgreSQL**, **Oracle**, and **MySQL/MariaDB**. Every query logged. Every connection tracked.

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

**Scopes** (optional): `api`, `auth`, `config`, `crypto`, `db`, `deps`, `docs`, `dump`, `grants`, `migrations`, `mysql`, `oracle`, `proxy`, `store`, `ui`, `release`

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
- **API**: `gin-gonic/gin` with OpenAPI 3.0 docs
- **CLI**: `urfave/cli/v3`
- **Config**: `knadh/koanf`
- **Logging**: `log/slog`
- **Frontend**: React 19 + TypeScript + Vite (see `front/CLAUDE.md`)
- **Dump format**: Protocol-agnostic binary capture (`docs/dump-format.md`)

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
│   ├── dump/                # Session packet dump format (read/write/anonymise)
│   ├── api/                 # REST API handlers and middleware
│   │   └── openapi.yml      # OpenAPI 3.0 specification
│   ├── proxy/
│   │   ├── shared/          # Auth, query interception shared across protocols
│   │   ├── postgresql/      # PostgreSQL wire protocol proxy
│   │   ├── oracle/          # Oracle TNS/TTC proxy (see docs/oracle.md)
│   │   └── mysql/           # MySQL/MariaDB proxy (see docs/mysql.md)
│   └── auth/                # OAuth provider abstraction (Slack, etc.)
├── front/                   # React frontend (see front/CLAUDE.md)
├── website/                 # Docusaurus site for dbbat.com
├── docs/                    # Protocol-level technical notes (oracle, mysql, dump format)
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

# Other
make demo             # Build and run in demo mode
make clean            # Clean build artifacts
```

## Development Sessions

**Never kill the running dbbat instance.** It is started beforehand with `make dev` which provides live reload (Air). Restarting it would break the dev workflow. The test mode credentials are `admin`/`admintest`.

## CLI Commands

```bash
./dbbat                            # Start server (default command)
./dbbat serve                      # Start server explicitly
./dbbat db migrate                 # Run pending migrations
./dbbat db rollback                # Rollback last migration group
./dbbat db status                  # Show migration status
./dbbat dump anonymise <in> [out]  # Strip session metadata from a .dbbat-dump
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Yes |
| `DBB_LISTEN_PG` | PostgreSQL proxy listen address (default: `:5434`) | No |
| `DBB_LISTEN_ORA` | Oracle proxy listen address (default: `:1522`; empty disables) | No |
| `DBB_LISTEN_MYSQL` | MySQL/MariaDB proxy listen address (default: `:3307`; empty disables) | No |
| `DBB_LISTEN_API` | REST API listen address (default: `:4200`) | No |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | No |
| `DBB_KEYFILE` | Path to file containing encryption key | No |
| `DBB_RUN_MODE` | Run mode: empty, `test`, or `demo` | No |
| `DBB_LOG_LEVEL` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) | No |
| `DBB_DUMP_DIR` | Directory for session dump files (empty = disabled) | No |
| `DBB_DUMP_MAX_SIZE` | Max dump file size per session in bytes (default: 10MB) | No |
| `DBB_DUMP_RETENTION` | Auto-delete dumps older than this (default: `24h`) | No |
| `DBB_MYSQL_TLS_DISABLE` | Refuse TLS upgrade on the MySQL listener (default: `false`) | No |
| `DBB_MYSQL_TLS_CERT_FILE` | PEM cert for MySQL TLS termination (auto self-signed if empty) | No |
| `DBB_MYSQL_TLS_KEY_FILE` | PEM RSA key for MySQL TLS termination (auto-generated if empty) | No |

Note: If no encryption key is provided, one is created at `~/.dbbat/key`.

## Testing

### Unit Tests
```bash
make test  # Uses testcontainers-go for PostgreSQL
```

### E2E Tests
```bash
make test-e2e  # Builds app, starts server in test mode, runs Playwright
```

Test mode credentials: `admin`/`admintest`, `viewer`/`viewer`, `connector`/`connector`

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
```

The same auth + grant + query-logging pipeline runs across all three protocols (`internal/proxy/shared`).

### Access Control
- Time-windowed grants (`starts_at`, `expires_at`)
- Controls: `read_only`, `block_copy`, `block_ddl` (combinable; empty = full write)
- Optional quotas: `max_query_counts`, `max_bytes_transferred`

### Security
- User passwords: Argon2id hashed
- Database credentials: AES-256-GCM encrypted (AAD-bound to the database UID)
- API keys: encrypted blobs, prefix `dbb_`; cannot create/revoke other keys
- Default admin: `admin`/`admin` (must change on first login)

## API Documentation

- OpenAPI spec: `internal/api/openapi.yml`
- Swagger UI: `GET /api/docs`
- All endpoints versioned under `/api/v1/`
