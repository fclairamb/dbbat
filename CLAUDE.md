# DBBat - PostgreSQL Observability Proxy

**Give your devs access to prod.**

A transparent PostgreSQL proxy for query observability, access control, and safety. Every query logged. Every connection tracked.

## Core Features

### User Management
- Users authenticate to DBBat with username/password
- Admin users can create/modify other users and manage all resources
- Default admin user created on first startup

### Database Configuration
- Store target database connection details (host, port, credentials, SSL mode)
- Credentials encrypted at rest with AES-256
- Each database configuration maps a DBBat database name to a target PostgreSQL server

### Connection & Query Tracking
- Track all connections (user, timestamp, source IP, target database)
- Track all queries: SQL text, execution time, rows affected
- Store query result data for audit/replay

### Access Control
- Grant time-windowed access (starts_at, expires_at) to specific databases
- Access levels: `read` or `write`
- Optional quotas per grant: max queries, max bytes transferred
- Revoke access manually or let it expire automatically
- Full audit log of all access control changes

## Usage Flow

1. Admin creates a user via REST API
2. Admin configures a target database via REST API
3. Admin grants the user access to the database (with optional time window and quotas)
4. User connects to DBBat with `psql` (or any PostgreSQL client) using their credentials
5. DBBat authenticates the user, checks grants, and proxies to the target database
6. All queries are logged and tracked

## Architecture

### Connection Flow
```
Client → DBBat (auth + grant check) → Target PostgreSQL
```

- The database name in the connection string determines the target database
- Example: User `florent` connects to DBBat with database `dev_metabase`
  → DBBat authenticates `florent`, checks for valid grant
  → DBBat connects to the target server using stored credentials
  → All queries are proxied and logged

### Admin Capabilities
- Create and modify users
- Create and modify database configurations (host, port, user, password, database name, description, SSL mode)
- Grant/revoke access to specific databases
- View connections, queries, and audit logs

## Security

- **User passwords**: Hashed with Argon2id (or bcrypt as fallback)
- **Database credentials**: Encrypted with AES-256-GCM
- **Encryption key**: From `DBB_KEY` env var (base64) or `DBB_KEYFILE` env var (path to key file)
- **Default admin**: Created on first startup with username `admin`, password `admin` (must be changed)

## Authentication

### Authentication Flow

Users must change their initial password before logging in. No session token is created until the password is changed.

```
New User Flow:
1. Admin creates user with temporary password
2. User attempts POST /api/v1/auth/login
3. Response: 403 { "error": "password_change_required" }
4. User calls PUT /api/v1/auth/password with username + old + new password
5. Password changed successfully
6. User calls POST /api/v1/auth/login
7. Success: token returned
```

### Token Types

| Type | Prefix | Purpose | Duration |
|------|--------|---------|----------|
| Web Session | `web_` | Frontend login (Bearer token) | 1 hour |
| API Key | `dbb_` | Programmatic access (Bearer token) | Configurable |

### Authentication Methods

| Method | Endpoints | Description |
|--------|-----------|-------------|
| Username/Password (JSON body) | `POST /auth/login`, `PUT /auth/password`, `PUT /users/:uid/password` | Credential-based authentication |
| Bearer Token (Web Session) | Most endpoints | Session token from login |
| Bearer Token (API Key) | Most endpoints except key management | Programmatic API access |

### Permissions by Auth Method

- **Web Sessions**: Can create/revoke API keys, full access to authorized resources
- **API Keys**: Cannot create/revoke API keys, otherwise same as web sessions
- **Non-admins**: Can only modify own resources
- **Viewers**: Can list all users and databases (read-only access for observability)

## Database Schema

See [docs/schema.sql](docs/schema.sql) for the complete database schema.

### Tables Overview
| Table | Purpose |
|-------|---------|
| `users` | DBBat user accounts (username, password hash, roles) |
| `api_keys` | API keys and web session tokens |
| `databases` | Target database configurations (host, port, encrypted credentials) |
| `connections` | Connection history through the proxy |
| `queries` | Query execution log with timing and results |
| `query_rows` | Stored query result and COPY data |
| `access_grants` | User-to-database access grants with quotas |
| `audit_logs` | Audit trail for access control changes |

## REST API

API documentation is provided via OpenAPI 3.0 specification:
- Specification file: `internal/api/openapi.yml`
- Served at: `GET /api/openapi.yml` (not versioned - documentation)
- Interactive docs: `GET /api/docs` (Swagger UI, not versioned)

All API endpoints are versioned under `/api/v1/`:

### Health & Version
- `GET /api/v1/health` - Health check
- `GET /api/v1/version` - Version and build information

### Auth
- `POST /api/v1/auth/login` - Login (returns web session token)
- `POST /api/v1/auth/logout` - Logout (revokes session)
- `GET /api/v1/auth/me` - Get current user and session info
- `PUT /api/v1/auth/password` - Change password (pre-login, no auth required)

### Users
- `POST /api/v1/users` - Create user (admin only)
- `GET /api/v1/users` - List users (admins see all, others see only themselves)
- `GET /api/v1/users/:uid` - Get user
- `PUT /api/v1/users/:uid` - Update user
- `PUT /api/v1/users/:uid/password` - Change password (credential auth in body)
- `DELETE /api/v1/users/:uid` - Delete user (admin only)

### API Keys
- `POST /api/v1/keys` - Create API key (web session or basic auth only)
- `GET /api/v1/keys` - List API keys
- `GET /api/v1/keys/:id` - Get API key
- `DELETE /api/v1/keys/:id` - Revoke API key (web session or basic auth only)

### Databases
- `POST /api/v1/databases` - Create database configuration (admin only)
- `GET /api/v1/databases` - List databases
- `GET /api/v1/databases/:uid` - Get database
- `PUT /api/v1/databases/:uid` - Update database (admin only)
- `DELETE /api/v1/databases/:uid` - Delete database (admin only)

### Grants
- `POST /api/v1/grants` - Create access grant (admin only)
- `GET /api/v1/grants` - List grants (filter by user/database)
- `GET /api/v1/grants/:uid` - Get grant
- `DELETE /api/v1/grants/:uid` - Revoke grant (admin only)

### Observability
- `GET /api/v1/connections` - List connections
- `GET /api/v1/queries` - List queries (with filters)
- `GET /api/v1/audit` - View audit log

## Technical Stack

- **Language**: Go
- **Storage**: PostgreSQL (same instance or separate)
- **ORM**: `uptrace/bun` - SQL-first ORM with type-safe query builder
- **Migrations**: `bun/migrate` - versioned SQL migrations with rollback support
- **Proxy**: Transparent TCP proxy using PostgreSQL wire protocol (via pgx)
- **Logging**: `log/slog` (standard library structured logging)
- **API Documentation**: OpenAPI 3.0 with Swagger UI
- **Libraries**:
  - `uptrace/bun` for database access and migrations
  - `jackc/pgx/v5` for PostgreSQL protocol (proxy wire protocol)
  - `gin-gonic/gin` for REST API
  - `urfave/cli/v2` for CLI argument parsing
  - `swaggo/gin-swagger` for serving interactive API docs

## Configuration

Configuration is managed using **koanf** with support for:
- Environment variables (prefix: `DBB_`)
- Configuration files (YAML, JSON, TOML)
- CLI flags (via urfave/cli)

Priority order: CLI flags > Environment variables > Config file > Defaults

### Environment Variables
| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_LISTEN_PG` | Proxy listen address (default: `:5432`) | No |
| `DBB_LISTEN_API` | REST API listen address (default: `:8080`) | No |
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Yes |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | One of KEY/KEYFILE |
| `DBB_KEYFILE` | Path to file containing encryption key | One of KEY/KEYFILE |
| `DBB_BASE_URL` | Base URL path for the frontend app (default: `/app`) | No |
| `DBB_REDIRECTS` | Dev redirect rules for proxying to dev servers (see below) | No |
| `DBB_RUN_MODE` | Run mode: empty (default) or `test` for test mode | No |

### Test Mode (DBB_RUN_MODE=test)

When `DBB_RUN_MODE=test` is set, the application performs the following on startup:

1. **Wipes all data** - Drops all tables (including migration tables) before running migrations (clean slate)
2. **Sets admin password** - Changes the admin password to `admintest` and marks it as changed
3. **Creates sample users**:
   - `viewer` (password: `viewer`) - Has viewer role only
   - `connector` (password: `connector`) - Has connector role only
4. **Creates sample database** - `proxy_target` pointing to the target database from docker-compose
5. **Creates sample grants**:
   - `connector` user gets **write** access to `proxy_target` (valid for 10 years)
   - `viewer` user gets **read** access to `proxy_target` (valid for 10 years)

This mode is useful for:
- E2E testing with Playwright
- Demo environments
- Development with a predictable initial state

**Example:**
```bash
DBB_RUN_MODE=test ./dbbat serve
```

**Test credentials:**
| Username | Password | Roles | Access |
|----------|----------|-------|--------|
| `admin` | `admintest` | admin, connector | Full access |
| `viewer` | `viewer` | viewer | Read-only to proxy_target |
| `connector` | `connector` | connector | Write to proxy_target |

### Development Redirects (DBB_REDIRECTS)

Enables proxying frontend requests to a dev server for hot module replacement.

**Format**: `/path:host:port/targetpath`

**Example**: `DBB_REDIRECTS=/app:localhost:5173/app` proxies `/app/*` to Vite dev server.

This is configured automatically when using `make dev`. See the Development section for details.

### Frontend Base URL (DBB_BASE_URL)

The frontend is served at `/app/` by default. To change this:

1. Set `DBB_BASE_URL` for the backend (e.g., `DBB_BASE_URL=/myapp`)
2. Build frontend with matching `VITE_BASE_URL` (e.g., `VITE_BASE_URL=/myapp ./scripts/build-frontend.sh`)

### Defaults
- Query retention: 30 days
- Connection retention: 30 days

## Development

### Quick Start

```bash
make dev            # Start full dev environment (frontend + backend with hot reload)
make dev-stop       # Stop all dev servers
```

This starts:
- **PostgreSQL** via docker-compose
- **Vite dev server** at `http://localhost:5173/app/` (frontend hot module replacement)
- **Air** at `http://localhost:8080` (backend hot reload with proxy to Vite)

Access the app at `http://localhost:8080/app/`

### Alternative: Separate Terminals

```bash
# Terminal 1: Frontend only
make dev-front

# Terminal 2: Backend only (requires frontend running for HMR)
make dev-back
```

### Build Commands

```bash
make build          # Build Go binary
make build-front    # Build frontend (to internal/api/resources/)
make build-all      # Build both frontend and backend
make test           # Run tests
make lint           # Run linter
make clean          # Clean build artifacts
```

### How Dev Proxy Works

The frontend is always served at `/app/` (both dev and production). During development:

1. Vite dev server runs at `http://localhost:5173/app/`
2. Backend runs at `http://localhost:8080` with `DBB_REDIRECTS=/app:localhost:5173/app`
3. Requests to `/app/*` are proxied to Vite, enabling hot module replacement
4. API requests to `/api/v1/*` are handled by the backend directly (versioned endpoints)
5. Documentation requests to `/api/openapi.yml` and `/api/docs` are also handled by the backend

### Testing Guidelines

Tests use standard Go testing with table-driven tests where appropriate:

```go
func TestExample(t *testing.T) {
    t.Parallel()

    result, err := SomeFunction()
    if err != nil {
        t.Fatalf("SomeFunction() error = %v", err)
    }
    if result != expected {
        t.Errorf("SomeFunction() = %v, want %v", result, expected)
    }
}
```

- Use `t.Parallel()` for tests that can run concurrently
- Use `t.Fatalf()` for errors that should stop the test immediately
- Use `t.Errorf()` for errors where the test can continue
- Use table-driven tests for testing multiple cases
- Store integration tests use **testcontainers-go** to spin up PostgreSQL containers automatically

## Project Structure

```
dbbat/
├── cmd/dbbat/
│   └── main.go              # Entry point with CLI commands (serve, db migrate, etc.)
├── internal/
│   ├── config/
│   │   └── config.go        # Environment config loading & validation
│   ├── crypto/
│   │   ├── hash.go          # Argon2id password hashing
│   │   └── encrypt.go       # AES-256-GCM for database credentials
│   ├── migrations/
│   │   ├── migrations.go    # Migration collection registration
│   │   └── sql/             # SQL migration files (up/down)
│   ├── store/
│   │   ├── store.go         # Bun DB connection, migration runner
│   │   ├── models.go        # Bun model definitions with struct tags
│   │   ├── users.go         # User CRUD
│   │   ├── api_keys.go      # API key and web session management
│   │   ├── databases.go     # Database config CRUD
│   │   ├── grants.go        # Grant CRUD + validation
│   │   ├── connections.go   # Connection logging
│   │   ├── queries.go       # Query logging + result storage
│   │   └── audit.go         # Audit log writes
│   ├── api/
│   │   ├── server.go        # Gin router setup
│   │   ├── middleware.go    # Auth middleware (Bearer, Basic Auth)
│   │   ├── auth.go          # Login, logout, password change handlers
│   │   ├── keys.go          # API key handlers
│   │   ├── users.go         # User handlers
│   │   ├── databases.go     # Database handlers
│   │   ├── grants.go        # Grant handlers
│   │   └── observability.go # Connections, queries, audit handlers
│   └── proxy/
│       ├── server.go        # TCP listener, accept loop
│       ├── session.go       # Per-connection handler
│       ├── auth.go          # DBBat authentication
│       ├── upstream.go      # Upstream PostgreSQL connection
│       └── intercept.go     # Query interception & logging
├── front/                   # React frontend (see front/CLAUDE.md)
├── specs/                   # Design specifications
├── docker-compose.yml
├── go.mod
└── CLAUDE.md
```

### CLI Commands

```bash
# Start DBBat server (default)
./dbbat
./dbbat serve

# Database migration commands
./dbbat db migrate   # Run pending migrations
./dbbat db rollback  # Rollback last migration group
./dbbat db status    # Show migration status
```

### Creating New Migrations

To create a new migration, add files to `internal/migrations/sql/`:

```
internal/migrations/sql/
├── 20260107000000_initial_schema.up.sql
├── 20260107000000_initial_schema.down.sql
├── YYYYMMDDHHMMSS_description.up.sql   # New migration (up)
└── YYYYMMDDHHMMSS_description.down.sql # New migration (down)
```

Use `--bun:split` directive to split multiple statements in a single migration file.

## Testing

### Unit & Integration Tests

Run all Go unit tests with:
```bash
make test
# or
go test ./...
```

Store integration tests use **testcontainers-go** to automatically spin up PostgreSQL containers. No external database setup required - Docker must be running.

### End-to-End Tests (Playwright)

Run automated E2E tests against the production build:
```bash
make test-e2e
```

This will:
1. Build the complete server (frontend + backend with embedded resources)
2. Start PostgreSQL via docker-compose
3. Run the server with `DBB_RUN_MODE=test` (creates admin/admintest credentials and sample data)
4. Execute Playwright tests against `http://localhost:8080/app/`
5. Tear down the server and PostgreSQL after tests complete

**Prerequisites:**
- Docker must be running (for PostgreSQL)
- Bun installed (for running Playwright)

**Individual browser tests:**
```bash
cd front
bun run test:e2e:chromium   # Chromium only
bun run test:e2e:firefox    # Firefox only
bun run test:e2e:webkit     # WebKit only
bun run test:e2e:ui         # Interactive UI mode
```

See `front/CLAUDE.md` for detailed E2E testing documentation.

### Manual/E2E Testing (docker-compose)

For manual/E2E testing, use docker-compose:

```bash
docker-compose up -d
```

This starts:
- **PostgreSQL** on port 5000 with:
  - `dbbat` database - DBBat schema
  - `target` database - Sample app with test tables
- **DBBat server** on port 5001 (proxy) and port 8080 (API)

#### Using Test Mode Users

In test mode, sample users with pre-changed passwords are available:

```bash
# Connect as connector user (has write access to proxy_target)
PGPASSWORD=connector psql -h localhost -p 5001 -U connector -d proxy_target -c "SELECT * FROM test_data"
```

#### Verify Connection Was Logged

```bash
# Login first to get a token
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "admintest"}' | jq -r '.token')

# List connections
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/connections | jq
```
