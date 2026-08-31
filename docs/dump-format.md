# DBBat Session Capture Format

Session captures are plain **pcapng** files. Anything that reads pcapng —
`tcpdump`, Wireshark, `tshark`, `capinfos`, `editcap`, scapy, `gopacket` — reads a
dbbat capture with no dbbat-specific tooling, and Wireshark applies its native
PostgreSQL, Oracle TNS, MySQL and MongoDB dissectors to the payloads.

The format is protocol-agnostic: PostgreSQL, Oracle, MySQL/MariaDB, MongoDB and
any future protocol all produce the same file structure.

## Design Principles

- **Standard on the wire** — no bespoke framing. Debugging a capture is a
  `tshark -r` away.
- **Single format for all protocols** — protocol-specific metadata lives in the
  session metadata blob, not in the framing.
- **Streamable writes** — packets are appended and flushed individually; a
  capture of a live session is readable while the session is still running.
- **Self-describing** — the session metadata is human-readable JSON and
  forward-compatible (unknown keys are ignored).

## File Extension

`.pcapng`

Captures are named `<connection-uid>.pcapng` inside `DBB_DUMP_DIR`, and keep
that name when uploaded to blob storage (see *Where captures are stored*).

## Why pcapng and not classic pcap

Classic pcap has a single global header, microsecond timestamps and no
per-packet metadata. pcapng gives us the three things this format needs:

| Need | pcapng feature |
|------|----------------|
| Session metadata (session id, protocol, connection details) | Section Header Block `opt_comment` |
| Per-packet direction | Enhanced Packet Block `epb_flags` |
| Nanosecond timestamps | Interface Description Block `if_tsresol = 9` |

## Block Layout

```
┌───────────────────────────────────────────────────────────┐
│ Section Header Block (SHB)                                │
│   shb_userappl = "dbbat"                                  │
│   opt_comment  = <session metadata JSON>                  │
├───────────────────────────────────────────────────────────┤
│ Interface Description Block (IDB)                         │
│   LinkType  = LINKTYPE_ETHERNET (1)                       │
│   if_name   = <protocol identifier>                       │
│   if_tsresol = 9  (nanoseconds)                           │
│   snaplen   = 262144                                      │
├───────────────────────────────────────────────────────────┤
│ Enhanced Packet Block  (one per captured payload)         │
│ Enhanced Packet Block                                     │
│ ...                                                       │
└───────────────────────────────────────────────────────────┘
```

There is no end-of-file marker: the capture ends at EOF, so a truncated or
still-growing file is still readable up to its last complete block.

The interface must advertise a non-zero snap length — Apple's libpcap rejects
captures whose interface declares an unlimited (`0`) snap length.

## Session Metadata

The session metadata is a single JSON object stored in the SHB `opt_comment`
option. Wireshark shows it under *Statistics → Capture File Properties*;
`capinfos` prints it as "Capture comment".

### Required Fields

| Field | Type | Description |
|-------|------|-------------|
| `session_id` | string | UUID identifying the proxy session |
| `protocol` | string | Protocol identifier (see below) |
| `start_time` | string | RFC 3339 timestamp with nanosecond precision |
| `connection` | object | Protocol-specific connection metadata |

`start_time` is the reference for every packet timestamp. pcapng timestamps are
unsigned offsets from the Unix epoch, so a zero or pre-epoch start time is
normalised to the write time.

### Protocol Identifiers

| Value | Protocol |
|-------|----------|
| `oracle` | Oracle TNS/TTC |
| `postgresql` | PostgreSQL wire protocol |
| `mysql` | MySQL / MariaDB |
| `mongodb` | MongoDB wire protocol |

New protocols add a new identifier. No format change needed.

### Connection Object

Protocol-specific. Readers must ignore unknown keys.

#### Oracle

| Field | Type | Description |
|-------|------|-------------|
| `service_name` | string | Oracle service name from the TNS connect descriptor |
| `upstream_addr` | string | Target Oracle address (`host:port`) |

```json
{
  "session_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "protocol": "oracle",
  "start_time": "2026-04-05T14:30:00.123456789Z",
  "connection": {
    "service_name": "ORCL",
    "upstream_addr": "10.0.0.1:1521"
  }
}
```

#### PostgreSQL

| Field | Type | Description |
|-------|------|-------------|
| `database` | string | Target database name |
| `user` | string | Connecting username |
| `upstream_addr` | string | Target PostgreSQL address (`host:port`) |

```json
{
  "session_id": "f9e8d7c6-b5a4-3210-fedc-ba0987654321",
  "protocol": "postgresql",
  "start_time": "2026-04-05T14:31:00.987654321Z",
  "connection": {
    "database": "myapp",
    "user": "readonly_user",
    "upstream_addr": "pg.internal:5432"
  }
}
```

### Extensibility

Additional top-level keys or connection keys may be added at any time. Readers
must tolerate and ignore unknown keys. Examples of future additions:

- `proxy_version` — dbbat version that wrote the file
- `connection.tls` — whether the upstream connection used TLS
- `connection.client_addr` — client source address
- `labels` — user-defined key/value tags

## Header Synthesis

dbbat taps the proxied connection at the application layer: it sees payload
bytes, not frames. To produce a file that standard tooling can dissect, every
captured payload is wrapped in a fabricated Ethernet / IPv4 / TCP header stack.

**None of the addressing is observed — all of it is synthesized.**

| Field | Value |
|-------|-------|
| Client MAC | `02:db:ba:70:00:01` (locally administered) |
| Server MAC | `02:db:ba:70:00:02` |
| Client IP | `10.77.0.1` |
| Client port | `54321` |
| Server IP | the upstream host from `connection.upstream_addr` when it is a literal IPv4 address, otherwise `10.77.0.2` |
| Server port | the upstream port from `connection.upstream_addr`, otherwise the protocol default (oracle 1521, postgresql 5432, mysql 3306, mongodb 27017) |
| IP TTL | 64 |
| TCP flags | `PSH` + `ACK` |
| TCP window | 65535 |
| Checksums | computed (IPv4 and TCP) |

Keeping the real server port matters: Wireshark's dissector heuristics key off
it, and that is what makes a capture show up as PGSQL / TNS / MySQL / Mongo
without a manual "Decode As".

### Direction

Direction is recorded twice, redundantly:

- **Addressing** — client→server frames go from the client endpoint to the
  server endpoint; server→client frames are the mirror image.
- **`epb_flags`** — bits 0-1 of the Enhanced Packet Block flags option, read
  from the proxy's point of view: `inbound` (`01`) for client→server, `outbound`
  (`10`) for server→client.

Readers prefer `epb_flags` and fall back to the source port.

### What a capture contains

A capture is forensic evidence, so the rule is *every frame the client actually
exchanged with dbbat* — not only the ones relayed to or from the upstream. Two
kinds of frame are easy to lose and are explicitly in scope:

- **A statement dbbat blocked.** It never reaches the upstream, so a proxy that
  records at the forwarding point records nothing at all.
- **A frame dbbat synthesized.** Every refusal — an Oracle OER, a MongoDB
  `Unauthorized` reply, a PostgreSQL `ErrorResponse`, a MySQL or TDS error — is
  written by dbbat, not read from the upstream, so a proxy that records at the
  relay point records the statement and then silence. That reads as a dropped
  connection rather than as an enforced control, which is the opposite of what
  the capture exists to show.

Each proxy therefore has **exactly one recording point per direction**, placed
where every frame of that direction passes, whatever its origin:

| Proxy | client→server | server→client |
|-------|---------------|---------------|
| PostgreSQL | client-socket tap (`dump.TapConn`) | same tap |
| MySQL/MariaDB | client-socket tap | same tap |
| SQL Server | client-leg TDS codec taps (`packetRW.setTaps`) | same |
| MongoDB | the read in `pumpClientToUpstream` | `writeClient` |
| Oracle | the reads in `clientToUpstream` + the fragment collector | client-socket **write** tap (`dump.NewWriteTapConn`) |

Oracle is the one split case. Its reader reassembles TNS packets out of a header
read plus a body read, and the fragment collector takes a single byte off the
stream and puts it back, so a read tap would record socket chunk boundaries
instead of TNS packets. The write side has no such split — one TNS packet is one
`Write` — so it is tapped, which is what makes it impossible for a refusal path
added later to answer the client without landing in the capture.

Only the **post-auth** phase is captured, on every protocol: the file is named
after the connection UID, and that row does not exist until authentication has
succeeded. A login dbbat refuses therefore has no capture at all, by
construction — the refusal is in `audit_log` and in the process log instead.

### Sequence numbers

Each direction keeps its own TCP sequence number, starting at 1 and advancing by
the payload length of every segment; the acknowledgement number is the peer
direction's next sequence number. Sequences are therefore contiguous and
gap-free, which is what lets Wireshark reassemble each half of the conversation
and run message-spanning dissectors over it.

No three-way handshake, FIN or RST is synthesized — the capture starts
mid-stream, which reassembly handles.

### Segmentation

A payload larger than 65495 bytes (65535 − 20 IPv4 header − 20 TCP header) does
not fit in one IPv4 datagram and is split across consecutive TCP segments with
advancing sequence numbers. Readers therefore see more packets than the writer
was handed; concatenating the segments of a direction reproduces the original
byte stream exactly.

## Timestamps

Every Enhanced Packet Block carries the absolute nanosecond timestamp
`start_time + <offset since session start>`. The dbbat reader re-derives the
relative offset by subtracting the `start_time` from the metadata.

## File Size Enforcement

Writers may enforce a maximum file size (`DBB_DUMP_MAX_SIZE`). When the next
packet's block would push the file past the limit, the writer silently skips it.
The limit applies to the whole file, headers included. A size-capped capture is
still a valid pcapng — it simply stops early.

## Where captures are stored

A capture is always **written to local disk first**, at
`$DBB_DUMP_DIR/<connection-uid>.pcapng`, and flushed after every packet. That is
what makes a capture readable even when the process dies mid-session: the file
on disk is a valid, if truncated, pcapng.

With `DBB_DUMP_UPLOAD_URL` set, the local directory becomes a **spool**. When
the session closes, the finished file is uploaded to the configured bucket and
the local copy is removed:

```
DBB_DUMP_UPLOAD_URL=s3://my-bucket/dbbat-captures
```

The scheme selects the driver (`gocloud.dev/blob`): `s3://` for S3 and
S3-compatible stores — credentials come from the standard AWS chain (instance
role, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, `~/.aws/credentials`) — and
`file://` for a local directory or a mounted volume. Any path after the bucket
is used as a key prefix.

The region comes from the environment (`AWS_REGION`) or from the URL:
`s3://my-bucket/captures?region=eu-west-1`. Other driver options travel the same
way — `&endpoint=…&awssdk=v2&use_path_style=true` for a MinIO or Ceph endpoint,
for instance. A bucket URL dbbat cannot open is a **startup failure**, on
purpose: silently falling back to local-only storage would look like it worked.

Object keys are `<prefix>/YYYY/MM/DD/<instance-id>/<connection-uid>.pcapng`. The
date segments exist for human browsing only: the key is recorded on the
`connections` row (`dump_key`), and that is what the API resolves a download
against — a capture is never located by listing the bucket.

Two consequences worth stating plainly:

- **Nothing is streamed to the bucket while a session is live.** S3 objects
  cannot be appended to, so a live stream would mean multipart uploads and an
  invisible, incomplete upload on a crash. The trade is that a capture is lost
  if the pod dies mid-session — an accepted limitation.
- **`DBB_DUMP_RETENTION` applies to the local spool only.** dbbat never expires
  an object it uploaded and never LISTs the bucket looking for one. Remote
  retention is the bucket's own lifecycle policy, and configuring it is part of
  deploying with an upload URL.

On startup, before any proxy accepts a connection, dbbat sweeps the spool and
uploads whatever is left there. Every file present at that moment belongs to a
previous run and is therefore finished, which is what makes the blind sweep
safe — and what recovers the captures of a run that crashed.

## Reading

With standard tooling:

```bash
tcpdump -nr <capture>.pcapng                       # packet list
capinfos <capture>.pcapng                          # shows the metadata as "Capture comment"
tshark -r <capture>.pcapng -V                      # full dissection
tshark -r <capture>.pcapng -z follow,tcp,raw,0     # reassembled byte stream
```

With `scripts/replay_dump.py` (no dependencies) for a dbbat-flavoured summary:

```bash
python3 scripts/replay_dump.py <capture>.pcapng
```

Programmatically, `internal/dump.Reader` yields `(RelativeNs, Direction, Data)`
per payload, stripping the synthesized headers back off.

## Anonymisation

`dbbat dump anonymise <input> [output]` produces a shareable copy:

- the session metadata is reduced to `session_id` and `protocol` — the
  connection object (database, user, service name, upstream address) is dropped;
- the capture is rebased onto the Unix epoch, so the wall-clock time of the
  session is gone while relative timing is preserved;
- **the synthesized IP addresses and ports are re-generated** from the fake
  endpoints, because the server side otherwise encodes the real upstream host
  and port. Pass `--keep-addresses` to opt out.

Payload bytes are never modified.
