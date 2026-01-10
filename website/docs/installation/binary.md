---
sidebar_position: 3
---

# Binary Installation

You can also run DBBat directly as a binary.

## Download

Download the latest release from [GitHub Releases](https://github.com/fclairamb/dbbat/releases).

## Building from Source

Ensure you have Go 1.21+ installed:

```bash
git clone https://github.com/fclairamb/dbbat.git
cd dbbat
go build -o dbbat ./cmd/dbbat
```

## Running

```bash
export DBB_DSN="postgres://user:pass@localhost:5432/dbbat"
export DBB_KEY="your-base64-encoded-key"

./dbbat serve
```

## CLI Commands

```bash
# Start DBBat server (default)
./dbbat
./dbbat serve

# Database migration commands
./dbbat db migrate   # Run pending migrations
./dbbat db rollback  # Rollback last migration group
./dbbat db status    # Show migration status
```

## Configuration File

DBBat supports YAML configuration files:

```yaml
# dbbat.yaml
listen_addr: ":5432"
api_addr: ":8080"
dsn: "postgres://user:pass@localhost:5432/dbbat"
```

Load with:

```bash
./dbbat serve --config dbbat.yaml
```

Priority order: CLI flags > Environment variables > Config file > Defaults

## Systemd Service

Create `/etc/systemd/system/dbbat.service`:

```ini
[Unit]
Description=DBBat PostgreSQL Proxy
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

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl enable dbbat
sudo systemctl start dbbat
```
