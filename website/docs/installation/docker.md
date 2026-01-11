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
  -p 8080:8080 \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat" \
  -e DBB_KEY="your-base64-encoded-key" \
  ghcr.io/fclairamb/dbbat
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_DSN` | PostgreSQL DSN for DBBat storage | Yes |
| `DBB_KEY` | Base64-encoded AES-256 encryption key | One of KEY/KEYFILE |
| `DBB_KEYFILE` | Path to file containing encryption key | One of KEY/KEYFILE |
| `DBB_LISTEN_PG` | Proxy listen address (default: `:5434`) | No |
| `DBB_LISTEN_API` | REST API listen address (default: `:8080`) | No |

## Generating an Encryption Key

Generate a secure 32-byte key:

```bash
openssl rand -base64 32
```

## Exposed Ports

- **5434**: PostgreSQL proxy port
- **8080**: REST API port

## Volumes

For persistent key storage, mount a volume:

```bash
docker run -d \
  --name dbbat \
  -p 5434:5434 \
  -p 8080:8080 \
  -e DBB_DSN="postgres://user:pass@host:5432/dbbat" \
  -e DBB_KEYFILE="/keys/dbbat.key" \
  -v /path/to/keys:/keys:ro \
  ghcr.io/fclairamb/dbbat
```

## Health Check

DBBat exposes a health endpoint:

```bash
curl http://localhost:8080/api/v1/health
```

## Next Steps

- [Configure DBBat](/docs/configuration)
- [Docker Compose setup](/docs/installation/docker-compose)
- [Kubernetes deployment](/docs/installation/kubernetes)
