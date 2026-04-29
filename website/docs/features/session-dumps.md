---
sidebar_position: 4
---

# Session Packet Dumps

DBBat can write **per-session binary dumps** of every proxied connection. The dump captures the post-auth byte stream between client and upstream, which is invaluable for protocol-level debugging, replay testing, and forensic analysis.

The format is **protocol-agnostic**: PostgreSQL, Oracle, and MySQL/MariaDB sessions all use the same `.dbbat-dump` file structure. See the [full format spec](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md) for the on-disk layout.

## Enabling Dumps

Dumps are **disabled by default**. Set `DBB_DUMP_DIR` to enable, optionally tuning size and retention.

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_DUMP_DIR` | Directory for `.dbbat-dump` files. Empty = disabled. | _disabled_ |
| `DBB_DUMP_MAX_SIZE` | Max bytes per session file. When the next packet would exceed this, it's silently skipped and an EOF marker is written. | `10485760` (10 MB) |
| `DBB_DUMP_RETENTION` | Auto-delete dumps older than this Go duration (`24h`, `7d`, `1h30m`, …). | `24h` |

```bash
docker run -d \
  -e DBB_DSN=... \
  -e DBB_DUMP_DIR=/var/dbbat/dumps \
  -e DBB_DUMP_MAX_SIZE=33554432 \
  -e DBB_DUMP_RETENTION=72h \
  -v dbbat-dumps:/var/dbbat/dumps \
  ghcr.io/fclairamb/dbbat
```

Each session writes a single file named `<connection-uid>.dbbat-dump`.

## What's Captured

The dump records the **post-authentication command stream** only:

- The MySQL/PostgreSQL/Oracle handshake and auth phase are **not** captured. Credentials, scrambles, and challenge data never reach the dump.
- TLS-upgraded connections (MySQL TLS termination) capture **TLS records** as they pass through the tap point — packet boundaries and timing are preserved, but contents are encrypted.
- Each packet records its direction (client→server / server→client), nanoseconds since session start, and raw bytes.

## File Format Highlights

```
┌──────────────────────────────────────────┐
│ Magic (DBBAT_DUMP)         16 bytes      │
│ Format version              2 bytes      │
│ JSON header length          4 bytes      │
├──────────────────────────────────────────┤
│ JSON header (session metadata)           │
├──────────────────────────────────────────┤
│ Packet 1   [13-byte frame] [data]        │
│ Packet 2   [13-byte frame] [data]        │
│ …                                        │
│ EOF marker [13-byte frame, dir=0xFF]     │
└──────────────────────────────────────────┘
```

The JSON header carries protocol-specific metadata (e.g. PostgreSQL `database`/`user`, Oracle `service_name`, upstream address). Readers must tolerate unknown keys — the format is forward-compatible.

## Anonymising a Dump for Sharing

Before sharing a dump (with vendors, support, an open-source maintainer), strip the connection metadata:

```bash
dbbat dump anonymise capture.dbbat-dump
# writes capture.anonymised.dbbat-dump

dbbat dump anonymise capture.dbbat-dump out.dbbat-dump
# explicit output path
```

The packet stream is preserved verbatim; only the JSON header is rewritten to remove identifying fields.

## Use Cases

- **Protocol-level debugging**: replay a captured Oracle TNS handshake to investigate `ORA-` errors, or diff two MySQL `caching_sha2_password` flows.
- **Regression tests**: feed a dump into the proxy's replay tests (see `internal/proxy/oracle/dump_replay_test.go` for an example).
- **Forensics**: confirm exactly which queries a compromised user ran, byte-for-byte, including any non-printable payload.

## Operational Notes

- Dumps disable themselves cleanly at process shutdown — the EOF marker is always written so partial captures remain readable.
- The `max_size` limit is enforced per file. There's no global cap, so monitor the dump directory growth and rely on `retention` for cleanup.
- For TLS-terminated MySQL connections, the tap sits **after** auth termination, so the dump sees plaintext command-phase traffic.
