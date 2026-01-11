# DBBat - PostgreSQL Observability Proxy

A transparent PostgreSQL proxy for query observability, access control, and safety. Every query logged. Every connection tracked.

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

**Scopes** (optional): `api`, `auth`, `config`, `crypto`, `db`, `deps`, `docs`, `grants`, `migrations`, `proxy`, `store`, `ui`, `release`

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
- **Proxy**: PostgreSQL wire protocol via `jackc/pgx/v5`
- **API**: `gin-gonic/gin` with OpenAPI 3.0 docs
- **CLI**: `urfave/cli/v3`
- **Config**: `knadh/koanf`
- **Logging**: `log/slog`
- **Frontend**: React 19 + TypeScript + Vite (see `front/CLAUDE.md`)

## Project Structure

```
dbbat/
├── cmd/dbbat/main.go        # Entry point with CLI commands
├── internal/
│   ├── config/              # Environment config loading
│   ├── crypto/              # Password hashing (Argon2id) + AES-256-GCM encryption
│   ├── migrations/sql/      # SQL migration files (up/down)
│   ├── store/               # Database models and CRUD operations
│   ├── api/                 # REST API handlers and middleware
│   │   └── openapi.yml      # OpenAPI 3.0 specification
│   └── proxy/               # PostgreSQL proxy (auth, upstream, query interception)
├── front/                   # React frontend (see front/CLAUDE.md)
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

## CLI Commands

```bash
./dbbat              # Start server (default command)
./dbbat serve        # Start server explicitly
./dbbat db migrate   # Run pending migrations
./dbbat db rollback  # Rollback last migration group
./dbbat db status    # Show migration status
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Yes |
| `DBB_LISTEN_PG` | Proxy listen address (default: `:5434`) | No |
| `DBB_LISTEN_API` | REST API listen address (default: `:8080`) | No |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | No |
| `DBB_KEYFILE` | Path to file containing encryption key | No |
| `DBB_RUN_MODE` | Run mode: empty, `test`, or `demo` | No |

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
```

### Access Control
- Time-windowed grants (starts_at, expires_at)
- Controls: `read_only`, `block_copy`, `block_ddl`
- Optional quotas: max queries, max bytes

### Security
- User passwords: Argon2id hashed
- Database credentials: AES-256-GCM encrypted
- Default admin: `admin`/`admin` (must change on first login)

## API Documentation

- OpenAPI spec: `internal/api/openapi.yml`
- Swagger UI: `GET /api/docs`
- All endpoints versioned under `/api/v1/`
