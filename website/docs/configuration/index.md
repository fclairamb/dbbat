---
sidebar_position: 1
---

# Configuration Overview

DBBat is configured via environment variables, an optional configuration file (YAML/JSON/TOML), or CLI flags.

## Priority Order

Configuration is loaded in this priority order (highest wins):

1. CLI flags
2. Environment variables (`DBB_…`)
3. Configuration file (`--config`, or `DBB_CONFIG=` env var)
4. Built-in defaults

## Environment Variables

### Required

| Variable | Description |
|----------|-------------|
| `DBB_DSN` | PostgreSQL DSN for DBBat's own storage (users, grants, queries, audit, …) |

### Listeners

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_LISTEN_PG` | PostgreSQL proxy listen address | `:5434` |
| `DBB_LISTEN_ORA` | Oracle proxy listen address. Empty value disables the Oracle proxy. | `:1522` |
| `DBB_LISTEN_MYSQL` | MySQL/MariaDB proxy listen address. Empty value disables it. | `:3307` |
| `DBB_LISTEN_API` | REST API + web UI listen address | `:4200` |

### Encryption Key

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_KEY` | Base64-encoded 32-byte AES-256 key | Auto-generated |
| `DBB_KEYFILE` | Path to a file containing the encryption key | - |

If neither is set, DBBat generates a key on first start and writes it to `~/.dbbat/key` (mode `0600`, parent dir `0700`). Losing this key means the encrypted database credentials cannot be recovered.

### Run Mode & Logging

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_RUN_MODE` | `` (production), `test`, or `demo` | `` |
| `DBB_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `DBB_BASE_URL` | Base URL path the frontend is served under | `/app` |
| `DBB_REDIRECTS` | Dev-only redirect rules (`/path:host:port[/target]`, comma-separated) | - |
| `DBB_DEMO_TARGET_DB` | Demo-mode allowed target (`user:pass@host/dbname`) | `demo:demo@localhost/demo` |

### Session Packet Dumps

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_DUMP_DIR` | Directory for `.dbbat-dump` files. Empty = disabled. | _disabled_ |
| `DBB_DUMP_MAX_SIZE` | Max dump file size per session, in bytes | `10485760` (10 MB) |
| `DBB_DUMP_RETENTION` | Auto-delete dumps older than this (Go duration) | `24h` |

See [Session Packet Dumps](/docs/features/session-dumps) for what gets captured.

### MySQL Proxy TLS

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_MYSQL_TLS_DISABLE` | Refuse `SSLRequest` packets and stay plaintext-only | `false` |
| `DBB_MYSQL_TLS_CERT_FILE` | PEM-encoded server certificate | _auto self-signed_ |
| `DBB_MYSQL_TLS_KEY_FILE` | PEM-encoded RSA private key (RSA required for the non-TLS `caching_sha2` public-key path) | _auto-generated RSA-2048_ |

### Query Result Storage

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_QUERY_STORAGE_STORE_RESULTS` | Globally enable result-row capture | `true` |
| `DBB_QUERY_STORAGE_MAX_RESULT_ROWS` | Max rows captured per query | `100000` |
| `DBB_QUERY_STORAGE_MAX_RESULT_BYTES` | Max bytes captured per query | `104857600` (100 MB) |

### Rate Limiting

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_RATE_LIMIT_ENABLED` | Enable per-user/IP rate limiting | `true` |
| `DBB_RATE_LIMIT_REQUESTS_PER_MINUTE` | Requests per minute per authenticated user | `60` |
| `DBB_RATE_LIMIT_REQUESTS_PER_MINUTE_ANON` | Requests per minute per source IP (unauthenticated) | `10` |
| `DBB_RATE_LIMIT_BURST` | Short-burst tolerance | `10` |

### Password Hashing (Argon2id)

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_HASH_PRESET` | One of `default`, `low`, `minimal` | `default` |
| `DBB_HASH_MEMORY_MB` | Memory cost (1–1024 MB) | `64` |
| `DBB_HASH_TIME` | Time cost (1–10) | `1` |
| `DBB_HASH_THREADS` | Parallelism (1–16) | `4` |

### Auth Cache

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_AUTH_CACHE_ENABLED` | Cache auth results across REST + proxies | `true` |
| `DBB_AUTH_CACHE_TTL_SECONDS` | Cache entry TTL | `300` |
| `DBB_AUTH_CACHE_MAX_SIZE` | Maximum cache entries | `10000` |

### Slack OAuth (optional)

| Variable | Description |
|----------|-------------|
| `DBB_SLACK_AUTH_CLIENT_ID` | Slack app client ID |
| `DBB_SLACK_AUTH_CLIENT_SECRET` | Slack app client secret |
| `DBB_SLACK_AUTH_TEAM_ID` | Restrict sign-in to one workspace |
| `DBB_SLACK_AUTH_AUTO_CREATE_USERS` | Auto-provision new users (default `true`) |
| `DBB_SLACK_AUTH_DEFAULT_ROLE` | Role assigned to auto-provisioned users (default `connector`) |

## Configuration File

DBBat supports YAML, JSON, and TOML configuration files.

### YAML Example

```yaml
listen_pg: ":5434"
listen_ora: ":1522"
listen_mysql: ":3307"
listen_api: ":4200"
dsn: "postgres://user:pass@localhost:5432/dbbat?sslmode=require"

query_storage:
  store_results: true
  max_result_rows: 100000
  max_result_bytes: 104857600

rate_limit:
  enabled: true
  requests_per_minute: 60
  burst: 10

dump:
  dir: "/var/dbbat/dumps"
  max_size: 33554432
  retention: "72h"

mysql:
  tls:
    disable: false
    cert_file: "/etc/dbbat/mysql.crt"
    key_file: "/etc/dbbat/mysql.key"

slack_auth:
  client_id: "..."
  client_secret: "..."
  auto_create_users: true
  default_role: "connector"
```

Load with the `--config` flag:

```bash
dbbat serve --config /etc/dbbat/config.yaml
```

## Generating an Encryption Key

DBBat requires a 32-byte AES-256 key for encrypting database credentials. If neither `DBB_KEY` nor `DBB_KEYFILE` is set, DBBat generates one at `~/.dbbat/key` and reuses it on subsequent starts.

To generate one yourself:

```bash
openssl rand -base64 32
```

Use it as `DBB_KEY=…` or write it to a file referenced by `DBB_KEYFILE=`.

## Storage Database

DBBat stores its configuration and logs in a PostgreSQL database. Provide the DSN via `DBB_DSN`.

### DSN Format

```
postgres://user:password@host:port/database?sslmode=require
```

### SSL Modes

- `disable` — No SSL
- `require` — Require SSL but don't verify certificate
- `verify-ca` — Require SSL and verify CA
- `verify-full` — Require SSL and verify CA + hostname

:::warning Security
DBBat warns at startup if any configured target database matches the storage DSN — sharing a database for storage and proxying enables privilege escalation. Use a separate database (or a separate cluster) for DBBat's own storage.
:::

## Run Modes

### Test Mode (`DBB_RUN_MODE=test`)

Useful for E2E testing and development:

- Wipes all DBBat-owned tables on startup
- Recreates admin with password `admintest` (already password-changed)
- Creates `viewer` (role `viewer`) and `connector` (role `connector`) users
- Creates a sample target database, plus stable API keys (`dbb_admin_key`, `dbb_viewer_key`, `dbb_connector_key`)

### Demo Mode (`DBB_RUN_MODE=demo`)

For public demos with restricted database targets:

- Wipes all DBBat-owned tables on startup
- Creates admin/viewer/connector users with their username as the password
- Only allows database configurations matching `DBB_DEMO_TARGET_DB`
- Defaults to `demo:demo@localhost/demo`

## Default Admin

On first startup (in production mode), DBBat creates a default admin user:

- **Username**: `admin`
- **Password**: `admin`

The password is flagged as requiring change. Login attempts return `403 password_change_required` until the admin calls `PUT /api/v1/auth/password` to set a real password.
