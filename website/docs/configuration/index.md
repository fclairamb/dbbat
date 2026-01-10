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
| `DBB_LISTEN_PG` | Proxy listen address | `:5434` | No |
| `DBB_LISTEN_API` | REST API listen address | `:8080` | No |
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | - | Yes |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | - | One of KEY/KEYFILE |
| `DBB_KEYFILE` | Path to file containing encryption key | - | One of KEY/KEYFILE |

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

DBBat requires a 32-byte encryption key for encrypting database credentials.

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

## Default Admin

On first startup, DBBat creates a default admin user:
- **Username**: `admin`
- **Password**: `admin`

**Important**: Change this password immediately after first login.
