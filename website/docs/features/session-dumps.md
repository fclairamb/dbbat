---
sidebar_position: 5
---

# Session Packet Captures

DBBat can write **per-session packet captures** of every proxied connection. The capture records the post-auth byte stream between client and upstream, which is invaluable for protocol-level debugging, replay testing, and forensic analysis.

Captures are plain **pcapng** files — the same format `tcpdump` and Wireshark write — so you can open one in Wireshark and get full PostgreSQL / Oracle TNS / MySQL / MongoDB / TDS dissection with no dbbat tooling involved.

The format is **protocol-agnostic**: PostgreSQL, Oracle, MySQL/MariaDB, MongoDB, and SQL Server sessions all use the same `.pcapng` structure. See the [full format spec](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md) for the block layout and header-synthesis rules.

## Enabling Captures

Captures are **disabled by default**. Set `DBB_DUMP_DIR` to enable, optionally tuning size and retention.

| Variable | Description | Default |
|----------|-------------|---------|
| `DBB_DUMP_DIR` | Directory for `.pcapng` files. Empty = disabled. | _disabled_ |
| `DBB_DUMP_MAX_SIZE` | Max bytes per session file. When the next packet would exceed this, it's silently skipped; the file stays a valid pcapng. | `10485760` (10 MB) |
| `DBB_DUMP_RETENTION` | Auto-delete captures older than this Go duration (`24h`, `7d`, `1h30m`, …). Applies to local captures only. | `24h` |
| `DBB_DUMP_UPLOAD_URL` | Blob bucket finished captures are uploaded to, e.g. `s3://my-bucket/captures`. Empty = keep captures on local disk. | _disabled_ |

```bash
docker run -d \
  -e DBB_DSN=... \
  -e DBB_DUMP_DIR=/var/dbbat/dumps \
  -e DBB_DUMP_MAX_SIZE=33554432 \
  -e DBB_DUMP_RETENTION=72h \
  -v dbbat-dumps:/var/dbbat/dumps \
  ghcr.io/fclairamb/dbbat
```

Each session writes a single file named `<connection-uid>.pcapng`.

## Storing Captures in S3

On Kubernetes, `DBB_DUMP_DIR` is ephemeral pod storage: captures die with the
pod and are invisible to the other replicas. Set `DBB_DUMP_UPLOAD_URL` to give
them a durable home.

```bash
docker run -d \
  -e DBB_DSN=... \
  -e DBB_DUMP_DIR=/var/dbbat/spool \
  -e DBB_DUMP_UPLOAD_URL=s3://my-bucket/dbbat-captures \
  ghcr.io/fclairamb/dbbat
```

`DBB_DUMP_DIR` then acts as a **spool**: the capture is still written to local
disk while the session is live, and uploaded once the session closes — at which
point the local copy is removed. Downloading a capture through the API or the UI
works the same either way; any replica can serve an uploaded capture, because
the object key is recorded on the connection row.

- The scheme selects the driver: `s3://` for S3 and S3-compatible stores
  (credentials come from the standard AWS chain), and `file://` for a mounted
  volume. `gs://` and `azblob://` work too.
- Objects are keyed `<prefix>/YYYY/MM/DD/<instance-id>/<connection-uid>.pcapng`.
- **`DBB_DUMP_RETENTION` does not apply to the bucket.** dbbat never expires an
  object it uploaded; configure a bucket lifecycle policy instead.
- Captures are never streamed to the bucket mid-session, so a pod that dies
  mid-session loses that session's capture. In exchange, a crash always leaves a
  valid partial capture on disk rather than an unusable incomplete upload.
- Anything left in the spool by a crashed run is uploaded at the next startup.

## What's Captured

The capture records the **post-authentication command stream** only:

- The MySQL/PostgreSQL/Oracle handshake and auth phase are **not** captured. Credentials, scrambles, and challenge data never reach the file.
- TLS-upgraded connections (MySQL TLS termination) capture **TLS records** as they pass through the tap point — packet boundaries and timing are preserved, but contents are encrypted.
- Each packet records its direction (client→server / server→client), a nanosecond timestamp, and the raw protocol bytes.

## How the pcapng Mapping Works

DBBat taps the connection at the application layer, so it only ever sees payload bytes. To produce a file standard tooling can dissect, each payload is wrapped in **synthesized** Ethernet / IPv4 / TCP headers:

- stable fabricated endpoints (`10.77.0.1:54321` on the client side);
- the **real upstream host and port** on the server side when known — this is what makes Wireshark's dissector heuristics fire and show the traffic as PGSQL / TNS / MySQL / Mongo;
- per-direction TCP sequence numbers that advance contiguously, so Wireshark can reassemble each half of the conversation.

Direction is also recorded explicitly in the per-packet `epb_flags` option, and the session metadata (session id, protocol, database/user/service name, upstream address) is a JSON blob in the Section Header Block comment — Wireshark shows it under *Statistics → Capture File Properties*.

```bash
tcpdump -nr <connection-uid>.pcapng     # packet list
capinfos <connection-uid>.pcapng        # session metadata, as "Capture comment"
tshark  -r <connection-uid>.pcapng -V   # full protocol dissection
```

## Anonymising a Capture for Sharing

Before sharing a capture (with vendors, support, an open-source maintainer), strip the identifying metadata:

```bash
dbbat dump anonymise capture.pcapng
# writes capture.anonymised.pcapng

dbbat dump anonymise capture.pcapng out.pcapng
# explicit output path
```

This drops the connection metadata (database, user, service name, upstream address), rebases the capture onto the Unix epoch so the session's wall-clock time is gone, and **rewrites the synthesized IP addresses and ports** — which would otherwise encode the real upstream host. Pass `--keep-addresses` to keep the original addressing. Payload bytes are preserved verbatim.

## Use Cases

- **Protocol-level debugging**: open a captured Oracle TNS handshake in Wireshark to investigate `ORA-` errors, or diff two MySQL `caching_sha2_password` flows.
- **Regression tests**: feed a capture into the proxy's replay tests (see `internal/proxy/oracle/dump_replay_test.go` for an example).
- **Forensics**: confirm exactly which queries a compromised user ran, byte-for-byte, including any non-printable payload.

## Operational Notes

- Blocks are flushed as they're written, and pcapng has no end-of-file marker, so a capture is readable while the session is still running and a partial capture stays valid.
- The `max_size` limit is enforced per file. There's no global cap, so monitor the capture directory growth and rely on `retention` for cleanup.
- For TLS-terminated MySQL connections, the tap sits **after** auth termination, so the capture sees plaintext command-phase traffic.
