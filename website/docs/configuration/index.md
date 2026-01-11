---
sidebar_position: 1
---

# Configuration Overview

DBBat is configured using environment variables, configuration files, or CLI flags.

## Priority Order

Configuration is loaded in this priority order (highest to lowest):
1. CLI flags
2. Environment variables
3. Configuration file
4. Default values

## Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | - | Yes |
| `DBB_LISTEN_PG` | Proxy listen address | `:5434` | No |
| `DBB_LISTEN_API` | REST API listen address | `:8080` | No |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | Auto-generated | No |
| `DBB_KEYFILE` | Path to file containing encryption key | - | No |
| `DBB_BASE_URL` | Base URL path for the frontend app | `/app` | No |
| `DBB_RUN_MODE` | Run mode: empty, `test`, or `demo` | - | No |
| `DBB_REDIRECTS` | Dev redirect rules for proxying to dev servers | - | No |
| `DBB_DEMO_TARGET_DB` | Allowed database target in demo mode | `demo:demo@localhost/demo` | No |

:::note
If no encryption key is provided via `DBB_KEY` or `DBB_KEYFILE`, DBBat automatically generates one and stores it at `~/.dbbat/key`.
:::

## Configuration File

DBBat supports YAML, JSON, and TOML configuration files.

### YAML Example

```yaml
listen_pg: ":5434"
listen_api: ":8080"
dsn: "postgres://user:pass@localhost:5432/dbbat?sslmode=require"
```

Load with the `--config` flag:

```bash
dbbat serve --config /etc/dbbat/config.yaml
```

## Encryption Key

DBBat requires a 32-byte encryption key for encrypting database credentials. If not provided, one is automatically generated.

### Generate a Key

```bash
openssl rand -base64 32
```

### Using Environment Variable

```bash
export DBB_KEY="YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY="
```

### Using Key File

```bash
openssl rand 32 > /etc/dbbat/key
chmod 600 /etc/dbbat/key
export DBB_KEYFILE="/etc/dbbat/key"
```

## Storage Database

DBBat stores its configuration and logs in a PostgreSQL database. You need to provide a DSN to this database.

### DSN Format

```
postgres://user:password@host:port/database?sslmode=require
```

### SSL Modes

- `disable` - No SSL
- `require` - Require SSL but don't verify certificate
- `verify-ca` - Require SSL and verify CA
- `verify-full` - Require SSL and verify CA + hostname

## Run Modes

### Test Mode (`DBB_RUN_MODE=test`)

Useful for E2E testing and development:
- Wipes all data on startup
- Creates admin user with password `admintest`
- Creates sample users: `viewer`, `connector`
- Creates sample database and grants

### Demo Mode (`DBB_RUN_MODE=demo`)

For public demos with restricted database targets:
- Wipes all data on startup
- Creates admin user with password `admin`
- Only allows database configurations matching `DBB_DEMO_TARGET_DB`

## Default Admin

On first startup, DBBat creates a default admin user:
- **Username**: `admin`
- **Password**: `admin`

**Important**: Change this password immediately after first login.
