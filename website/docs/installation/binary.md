---
sidebar_position: 3
---

# Binary Installation

You can also run DBBat directly as a binary.

## Download

Download the latest release from [GitHub Releases](https://github.com/fclairamb/dbbat/releases). Builds are produced via goreleaser for Linux/macOS on amd64 and arm64.

## Building from Source

Ensure you have Go 1.26+ and Bun (for the embedded frontend). Then:

```bash
git clone https://github.com/fclairamb/dbbat.git
cd dbbat
make build-app   # builds frontend + backend, embeds the UI in the binary
```

The resulting binary is at `./dbbat`.

## Running

```bash
export DBB_DSN="postgres://user:pass@localhost:5432/dbbat"
export DBB_KEY="your-base64-encoded-key"

./dbbat serve
```

By default, DBBat listens on:

| Service | Address |
|---------|---------|
| PostgreSQL proxy | `:5434` |
| Oracle proxy | `:1522` |
| MySQL / MariaDB proxy | `:3307` |
| REST API + web UI | `:4200` |

Set `DBB_LISTEN_*=""` to disable a listener you don't need.

## CLI Commands

```bash
# Start the server (default action)
./dbbat
./dbbat serve

# Database migrations
./dbbat db migrate    # Run pending migrations
./dbbat db rollback   # Rollback the last migration group
./dbbat db status     # Show migration status

# Dump utilities
./dbbat dump anonymise capture.dbbat-dump            # writes capture.anonymised.dbbat-dump
./dbbat dump anonymise capture.dbbat-dump out.dump   # explicit output path
```

CLI flags override env vars; env vars override the config file. Common flags:

```bash
./dbbat serve \
  --listen-addr :5434 \
  --api-addr :4200 \
  --dsn "postgres://user:pass@localhost:5432/dbbat" \
  --keyfile /etc/dbbat/key
```

## Configuration File

DBBat supports YAML, JSON, and TOML configuration files:

```yaml
# /etc/dbbat/config.yaml
listen_pg: ":5434"
listen_ora: ":1522"
listen_mysql: ":3307"
listen_api: ":4200"
dsn: "postgres://user:pass@localhost:5432/dbbat"

dump:
  dir: "/var/dbbat/dumps"
  retention: "72h"
```

Load with:

```bash
./dbbat serve --config /etc/dbbat/config.yaml
```

Priority order: **CLI flags > environment variables > config file > defaults**.

## Systemd Service

Create `/etc/systemd/system/dbbat.service`:

```ini
[Unit]
Description=DBBat Database Observability Proxy
After=network.target postgresql.service

[Service]
Type=simple
User=dbbat
Group=dbbat
ExecStart=/usr/local/bin/dbbat serve
Restart=on-failure
RestartSec=5
Environment=DBB_DSN=postgres://user:pass@localhost:5432/dbbat
Environment=DBB_KEYFILE=/etc/dbbat/key
Environment=DBB_LISTEN_PG=:5434
Environment=DBB_LISTEN_ORA=:1522
Environment=DBB_LISTEN_MYSQL=:3307
Environment=DBB_LISTEN_API=:4200

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl enable --now dbbat
```

## Next Steps

- [Configure DBBat](/docs/configuration)
- [Docker installation](/docs/installation/docker)
- [Kubernetes deployment](/docs/installation/kubernetes)
