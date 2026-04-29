# DBBat - Database Observability Proxy

**Give your devs access to prod.**

A transparent database proxy for query observability, access control, and safety. Speaks **PostgreSQL**, **Oracle**, and **MySQL/MariaDB** wire protocols. Every query logged. Every connection tracked.

## Documentation

Full documentation is available at **[dbbat.com](https://dbbat.com)**:
- [Getting Started](https://dbbat.com/docs/intro)
- [Installation](https://dbbat.com/docs/installation/docker)
- [Configuration](https://dbbat.com/docs/configuration)
- [API Reference](https://dbbat.com/docs/api)

## Why DBBat?

**The Problem:**
- Production databases should not be directly accessible to developers for security and compliance reasons
- Developers often need access to production data to diagnose issues, debug problems, and understand user behavior
- Traditional solutions are binary: either full access (risky) or no access (blocks troubleshooting)

**The Solution:**

DBBat acts as a monitoring proxy that allows controlled developer access to production databases with:
- **Complete monitoring**: Every query and result is logged with full traceability
- **Strict limitations**: Time-windowed access, fine-grained controls, query quotas, and data transfer limits
- **Full audit trail**: Track who accessed what, when, and what data they retrieved
- **Encrypted credentials**: Database passwords never exposed to users
- **Granular access control**: Grant temporary access to specific databases with precise permissions

## Supported Databases

| Engine | Protocol | Default proxy port | Notes |
|--------|----------|--------------------|-------|
| PostgreSQL | PostgreSQL wire (`pgx/v5`) | `:5434` (`DBB_LISTEN_PG`) | First-class support; auth terminated at proxy |
| Oracle | TNS / TTC | `:1522` (`DBB_LISTEN_ORA`) | O5LOGON proxy auth; `go-ora` end-to-end (other clients reach AUTH only — see [docs/oracle.md](docs/oracle.md)) |
| MySQL | MySQL wire (`go-mysql-org/go-mysql`) | `:3307` (`DBB_LISTEN_MYSQL`) | `caching_sha2_password` (default), `mysql_clear_password`; TLS terminated at proxy |
| MariaDB | MySQL wire (same listener) | `:3307` (`DBB_LISTEN_MYSQL`) | Same as MySQL — `mysql_native_password` not supported, `STMT_BULK_EXECUTE` refused |

Each engine has its own listener; enable only the ones you need by setting the matching `DBB_LISTEN_*` environment variable. PostgreSQL is enabled by default; Oracle/MySQL listen on their default ports unless explicitly disabled in config.

## Features

- **Multi-engine proxy**: PostgreSQL, Oracle, MySQL/MariaDB on independent listeners
- **User Management**: Local user database with username/password (Argon2id) and `admin`/`viewer`/`connector` roles
- **API Keys**: Long-lived bearer tokens (`dbb_…`) for programmatic access; cannot create or revoke other keys (security restriction)
- **Slack OAuth (optional)**: Sign-in via Slack workspace, optional auto-provisioning
- **Database Configuration**: Store target database connections with AES-256-GCM encrypted credentials; `protocol` field per database (`postgresql`, `oracle`, `mysql`, `mariadb`)
- **Connection & Query Tracking**: Logs every connection, every query (SQL text, parameters, duration, rows affected, errors), and optionally captures result rows (`query_rows` table) up to configurable size limits
- **Access Control**: Time-windowed grants (`starts_at` / `expires_at`), independent controls (`read_only`, `block_copy`, `block_ddl`), and optional quotas (`max_query_counts`, `max_bytes_transferred`)
- **Read-only enforcement**: Defense in depth — SQL inspection, PostgreSQL `default_transaction_read_only`, MySQL/MariaDB blocks for `LOAD DATA`/`SELECT … INTO OUTFILE`/etc., and proxy-side opt-out from `LOCAL INFILE`
- **Audit Trail**: Append-only audit log of user, grant, and database changes
- **Rate Limiting**: Per-user request limits and exponential backoff on failed login
- **Authentication Cache**: Optional in-memory cache (TTL + max size) shared across REST and proxy auth paths
- **Session Packet Dumps**: Optional binary capture of post-auth session traffic (`.dbbat-dump` files); same format across all protocols (see [docs/dump-format.md](docs/dump-format.md)) with `dbbat dump anonymise` for sharing
- **REST API**: OpenAPI 3.0 documented (`/api/docs`), versioned under `/api/v1/`
- **Web UI**: Embedded React frontend served at `/app`
- **Demo / Test modes**: Self-provisioning sample data for safe trials and E2E testing

## Quick Start

### Running with Docker

```bash
docker run -d \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat?sslmode=require" \
  -p 5434:5434 \
  -p 1522:1522 \
  -p 3307:3307 \
  -p 4200:4200 \
  ghcr.io/fclairamb/dbbat
```

Ports: `5434` PostgreSQL proxy, `1522` Oracle proxy, `3307` MySQL/MariaDB proxy, `4200` REST API + web UI.

### Running with Docker Compose

See [docker-compose installation](https://dbbat.com/docs/installation/docker-compose) for a complete example.

## Usage Example

All API endpoints are under `/api/v1/`. See the [API Reference](https://dbbat.com/docs/api) for complete documentation.

### 1. Login and get a token

```bash
TOKEN=$(curl -s -X POST http://localhost:4200/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "admin"}' | jq -r '.token')
```

### 2. Create a User

```bash
curl -X POST http://localhost:4200/api/v1/users \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "username": "developer",
    "password": "temppass123",
    "roles": ["connector"]
  }'
```

### 3. Configure a Target Database

```bash
curl -X POST http://localhost:4200/api/v1/databases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production",
    "description": "Production database",
    "protocol": "postgresql",
    "host": "db.example.com",
    "port": 5432,
    "database_name": "myapp",
    "username": "readonly_user",
    "password": "dbpass",
    "ssl_mode": "require"
  }'
```

For Oracle, set `"protocol": "oracle"` and add `"oracle_service_name": "ORCL"`. For MySQL/MariaDB, set `"protocol": "mysql"` (or `"mariadb"`) and use port `3306`.

### 4. Grant Access

```bash
curl -X POST http://localhost:4200/api/v1/grants \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": "<user-uid>",
    "database_id": "<database-uid>",
    "controls": ["read_only"],
    "starts_at": "2024-01-01T00:00:00Z",
    "expires_at": "2024-12-31T23:59:59Z",
    "max_query_counts": 1000,
    "max_bytes_transferred": 10485760
  }'
```

`controls` accepts any combination of `read_only`, `block_copy`, `block_ddl`. An empty array means full write access.

### 5. Connect via Proxy

```bash
# PostgreSQL
psql -h localhost -p 5434 -U developer -d production

# Oracle (go-ora-style easy connect)
# Use the database name (or its oracle_service_name) as SERVICE_NAME
# Example with sqlplus: developer/temppass123@//localhost:1522/production

# MySQL / MariaDB
mysql -h 127.0.0.1 -P 3307 -u developer -p production
```

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Required |
| `DBB_LISTEN_PG` | PostgreSQL proxy listen address | `:5434` |
| `DBB_LISTEN_ORA` | Oracle proxy listen address (empty disables) | `:1522` |
| `DBB_LISTEN_MYSQL` | MySQL/MariaDB proxy listen address (empty disables) | `:3307` |
| `DBB_LISTEN_API` | REST API listen address | `:4200` |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | Auto-generated at `~/.dbbat/key` |
| `DBB_KEYFILE` | Path to file containing encryption key | - |
| `DBB_RUN_MODE` | Run mode: empty (production), `test`, or `demo` | - |
| `DBB_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `DBB_DUMP_DIR` | Directory for session packet dumps (empty disables) | - |
| `DBB_DUMP_MAX_SIZE` | Max dump file size per session, in bytes | `10485760` (10 MB) |
| `DBB_DUMP_RETENTION` | Auto-delete dumps older than this (Go duration) | `24h` |
| `DBB_MYSQL_TLS_DISABLE` | Disable MySQL TLS termination at the proxy | `false` |
| `DBB_MYSQL_TLS_CERT_FILE` | PEM cert for MySQL TLS (auto self-signed if empty) | - |
| `DBB_MYSQL_TLS_KEY_FILE` | PEM RSA key for MySQL TLS (auto-generated if empty) | - |

See [Configuration](https://dbbat.com/docs/configuration) for the full set, including rate limiting, query storage, hash presets, auth cache, Slack OAuth, demo target, and dev redirects.

## Security

- User passwords are hashed with Argon2id
- Database credentials are encrypted with AES-256-GCM (AAD-bound to the database UID)
- API keys (`dbb_…`) are stored as encrypted blobs and cannot create or revoke other keys
- Failed logins trigger per-username exponential backoff
- Default admin user (`admin` / `admin`) is created on first startup — **change it immediately**

## Architecture

```
psql / pg client     ─►  DBBat (auth + grant check + log) ─► PostgreSQL upstream
sqlplus / go-ora     ─►  DBBat (TNS service-name routing)  ─► Oracle upstream
mysql / mariadb cli  ─►  DBBat (caching_sha2_password)     ─► MySQL / MariaDB upstream
```

DBBat is a single Go binary backed by a PostgreSQL store (users, databases, grants, connections, queries, audit, dumps).

## Development

```bash
make dev          # Start dev environment with hot reload (Air + Vite)
make test         # Run Go tests (uses testcontainers)
make test-e2e     # Run Playwright E2E tests
make build-app    # Build frontend + backend
make lint         # Run golangci-lint
```

See [CLAUDE.md](CLAUDE.md) for development documentation.

## License

AGPL-3.0
