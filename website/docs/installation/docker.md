---
sidebar_position: 1
---

# Docker Installation

The easiest way to run DBBat is with Docker.

## Quick Start

```bash
docker run -d \
  --name dbbat \
  -p 5434:5434 \
  -p 1522:1522 \
  -p 3307:3307 \
  -p 4200:4200 \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat" \
  -e DBB_KEY="your-base64-encoded-key" \
  ghcr.io/fclairamb/dbbat
```

The same container exposes all four listeners — PostgreSQL, Oracle, MySQL/MariaDB, and the REST API + web UI. Drop the `-p` mapping for any proxy you don't need, and disable that listener with the matching `DBB_LISTEN_*=""` env var.

## Environment Variables

### Core

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Yes |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | One of `KEY`/`KEYFILE` (auto-generated if neither) |
| `DBB_KEYFILE` | Path to file containing encryption key | One of `KEY`/`KEYFILE` |

### Listeners

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_LISTEN_PG` | PostgreSQL proxy listen address | `:5434` |
| `DBB_LISTEN_ORA` | Oracle proxy listen address (empty = disabled) | `:1522` |
| `DBB_LISTEN_MYSQL` | MySQL/MariaDB proxy listen address (empty = disabled) | `:3307` |
| `DBB_LISTEN_API` | REST API + web UI listen address | `:4200` |

For the full list (rate limiting, dump capture, MySQL TLS, hashing, Slack OAuth), see [Configuration](/docs/configuration).

## Generating an Encryption Key

Generate a secure 32-byte key:

```bash
openssl rand -base64 32
```

If you don't provide one via `DBB_KEY` or `DBB_KEYFILE`, DBBat creates one at `~/.dbbat/key` (inside the container) on first start. Mount that path as a volume to keep it across restarts.

## Exposed Ports

| Port | Purpose |
|------|---------|
| `5434` | PostgreSQL proxy |
| `1522` | Oracle proxy |
| `3307` | MySQL / MariaDB proxy |
| `4200` | REST API + web UI |

## Volumes

For persistent key storage, mount a volume:

```bash
docker run -d \
  --name dbbat \
  -p 5434:5434 -p 1522:1522 -p 3307:3307 -p 4200:4200 \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat" \
  -e DBB_KEYFILE="/keys/dbbat.key" \
  -v /path/to/keys:/keys:ro \
  ghcr.io/fclairamb/dbbat
```

To enable session packet dumps:

```bash
docker run -d \
  --name dbbat \
  -p 5434:5434 -p 1522:1522 -p 3307:3307 -p 4200:4200 \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat" \
  -e DBB_DUMP_DIR="/var/dbbat/dumps" \
  -e DBB_DUMP_RETENTION="72h" \
  -v dbbat-dumps:/var/dbbat/dumps \
  ghcr.io/fclairamb/dbbat
```

## Health Check

DBBat exposes a health endpoint:

```bash
curl http://localhost:4200/api/v1/health
```

## Next Steps

- [Configure DBBat](/docs/configuration)
- [Docker Compose setup](/docs/installation/docker-compose)
- [Kubernetes deployment](/docs/installation/kubernetes)
