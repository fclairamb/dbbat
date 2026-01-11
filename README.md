# DBBat - PostgreSQL Observability Proxy

**Give your devs access to prod.**

A transparent PostgreSQL proxy for query observability, access control, and safety. Every query logged. Every connection tracked.

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
- **Strict limitations**: Time-windowed access, read/write controls, query quotas, and data transfer limits
- **Full audit trail**: Track who accessed what, when, and what data they retrieved
- **Encrypted credentials**: Database passwords never exposed to users
- **Granular access control**: Grant temporary access to specific databases with precise permissions

## Features

- **User Management**: Authenticate users with username/password, role-based access control
- **Database Configuration**: Store target database connections with encrypted credentials
- **Connection & Query Tracking**: Log all connections and queries with timing and results
- **Access Control**: Time-windowed access grants with controls (`read_only`, `block_copy`, `block_ddl`) and quotas
- **REST API**: Full API for management and observability
- **PostgreSQL Proxy**: Transparent proxy with wire protocol support

## Quick Start

### Running with Docker

```bash
docker run -d \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat?sslmode=require" \
  -p 5434:5434 \
  -p 8080:8080 \
  ghcr.io/fclairamb/dbbat
```

### Running with Docker Compose

See [docker-compose installation](https://dbbat.com/docs/installation/docker-compose) for a complete example.

## Usage Example

All API endpoints are under `/api/v1/`. See the [API Reference](https://dbbat.com/docs/api) for complete documentation.

### 1. Login and get a token

```bash
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "admin"}' | jq -r '.token')
```

### 2. Create a User

```bash
curl -X POST http://localhost:8080/api/v1/users \
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
curl -X POST http://localhost:8080/api/v1/databases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production",
    "description": "Production database",
    "host": "db.example.com",
    "port": 5432,
    "database_name": "myapp",
    "username": "readonly_user",
    "password": "dbpass",
    "ssl_mode": "require"
  }'
```

### 4. Grant Access

```bash
curl -X POST http://localhost:8080/api/v1/grants \
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

### 5. Connect via Proxy

```bash
psql -h localhost -p 5434 -U developer -d production
```

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Required |
| `DBB_LISTEN_PG` | Proxy listen address | `:5434` |
| `DBB_LISTEN_API` | REST API listen address | `:8080` |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | Auto-generated |
| `DBB_KEYFILE` | Path to file containing encryption key | - |
| `DBB_RUN_MODE` | Run mode: empty, `test`, or `demo` | - |

See [Configuration](https://dbbat.com/docs/configuration) for all options.

## Security

- User passwords are hashed with Argon2id
- Database credentials are encrypted with AES-256-GCM
- Default admin user (username: `admin`, password: `admin`) is created on first startup - **change this immediately!**

## Architecture

```
Client -> DBBat (auth + grant check) -> Target PostgreSQL
```

## Development

```bash
make dev          # Start dev environment with hot reload
make test         # Run tests
make build-app    # Build frontend + backend
make lint         # Run linter
```

See [CLAUDE.md](CLAUDE.md) for development documentation.

## License

AGPL-3.0
