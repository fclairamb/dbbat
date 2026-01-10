# PgLens - PostgreSQL Observability Proxy

**Give your devs access to prod.**

A transparent PostgreSQL proxy for query observability, access control, and safety. Every query logged. Every connection tracked.

## Why PgLens?

**The Problem:**
- Production databases should not be directly accessible to developers for security and compliance reasons
- Developers often need access to production data to diagnose issues, debug problems, and understand user behavior
- Traditional solutions are binary: either full access (risky) or no access (blocks troubleshooting)

**The Solution:**

PgLens acts as a monitoring proxy that allows controlled developer access to production databases with:
- **Complete monitoring**: Every query and result is logged with full traceability
- **Strict limitations**: Time-windowed access, read/write controls, query quotas, and data transfer limits
- **Full audit trail**: Track who accessed what, when, and what data they retrieved
- **Encrypted credentials**: Database passwords never exposed to users
- **Granular access control**: Grant temporary access to specific databases with precise permissions

PgLens gives you the best of both worlds: developers can troubleshoot production issues while you maintain complete visibility and control.

## Features

- **User Management**: Authenticate users with username/password, admin capabilities
- **Database Configuration**: Store target database connections with encrypted credentials
- **Connection & Query Tracking**: Log all connections and queries with timing and results
- **Access Control**: Time-windowed access grants with read/write levels and quotas
- **REST API**: Full API for management and observability
- **PostgreSQL Proxy**: Transparent proxy with wire protocol support

## Quick Start

### Prerequisites

- Go 1.21+
- PostgreSQL 15+
- Docker & Docker Compose (for testing)

### Running with Docker Compose

```bash
# Start the services
docker-compose up -d

# The following services will be available:
# - PostgreSQL: localhost:5000
# - PgLens Proxy: localhost:5001
# - PgLens API: localhost:8080
```

### Running Locally

1. **Set up a PostgreSQL database** for PgLens storage:
```bash
createdb pglens
```

2. **Generate an encryption key** (32 bytes, base64-encoded):
```bash
openssl rand -base64 32
```

3. **Set environment variables**:
```bash
export PGL_DSN="postgres://user:password@localhost:5432/pglens?sslmode=disable"
export PGL_KEY="<your-base64-encoded-key>"
export PGL_LISTEN_ADDR=":5432"
export PGL_API_ADDR=":8080"
```

4. **Build and run**:
```bash
go build -o pglens ./cmd/pglens
./pglens
```

## Usage

### 1. Create a User

```bash
curl -u admin:admin -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{
    "username": "john",
    "password": "secret123",
    "is_admin": false
  }'
```

### 2. Configure a Target Database

```bash
curl -u admin:admin -X POST http://localhost:8080/api/databases \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my_database",
    "description": "Production database",
    "host": "localhost",
    "port": 5432,
    "database_name": "myapp",
    "username": "dbuser",
    "password": "dbpass",
    "ssl_mode": "prefer"
  }'
```

### 3. Grant Access

```bash
curl -u admin:admin -X POST http://localhost:8080/api/grants \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 2,
    "database_id": 1,
    "access_level": "read",
    "starts_at": "2024-01-01T00:00:00Z",
    "expires_at": "2024-12-31T23:59:59Z",
    "max_queries": 1000,
    "max_bytes": 10485760
  }'
```

### 4. Connect via Proxy

```bash
psql -h localhost -p 5001 -U john -d my_database
```

### 5. View Query Logs

```bash
curl -u admin:admin http://localhost:8080/api/queries?limit=10
```

## API Endpoints

### Users
- `POST /api/users` - Create user
- `GET /api/users` - List users
- `GET /api/users/:id` - Get user
- `PUT /api/users/:id` - Update user
- `DELETE /api/users/:id` - Delete user

### Databases
- `POST /api/databases` - Create database
- `GET /api/databases` - List databases
- `GET /api/databases/:id` - Get database
- `PUT /api/databases/:id` - Update database
- `DELETE /api/databases/:id` - Delete database

### Grants
- `POST /api/grants` - Create grant
- `GET /api/grants` - List grants
- `GET /api/grants/:id` - Get grant
- `DELETE /api/grants/:id` - Revoke grant

### Observability
- `GET /api/connections` - List connections
- `GET /api/queries` - List queries
- `GET /api/queries/:id` - Get query with result rows
- `GET /api/audit` - List audit events

## Configuration

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `PGL_LISTEN_ADDR` | Proxy listen address | No | `:5432` |
| `PGL_API_ADDR` | REST API listen address | No | `:8080` |
| `PGL_DSN` | PostgreSQL DSN for PgLens storage | Yes | - |
| `PGL_KEY` | Base64-encoded AES-256 encryption key | Yes* | - |
| `PGL_KEYFILE` | Path to file containing encryption key | Yes* | - |

*Either `PGL_KEY` or `PGL_KEYFILE` must be set.

## Development

```bash
# Build
go build ./...

# Test
go test ./...

# Lint
golangci-lint run
```

## Security

- User passwords are hashed with Argon2id
- Database credentials are encrypted with AES-256-GCM
- Default admin user (username: `admin`, password: `admin`) is created on first startup - **change this immediately!**

## Architecture

```
Client → PgLens (auth + grant check) → Target PostgreSQL
```

See [CLAUDE.md](CLAUDE.md) for detailed architecture and implementation documentation.

## License

MIT
