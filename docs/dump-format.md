# DBBat Dump Format v2

Binary capture format for database proxy sessions. Protocol-agnostic: the same format works for PostgreSQL, Oracle, and any future protocol.

## Design Principles

- **Single format for all protocols** — protocol-specific metadata lives in a JSON header, not in the binary framing.
- **Streamable writes** — packets are appended individually, no buffering required.
- **Compact** — packet data stays binary. No base64 bloat.
- **Self-describing** — JSON header is human-readable and forward-compatible (unknown keys are ignored).

## File Extension

`.dbbat-dump` (unchanged from v1)

## Layout

```
┌──────────────────────────────────────────┐
│ Magic              16 bytes              │
│ Format version      2 bytes              │
│ Header length       4 bytes              │
├──────────────────────────────────────────┤
│ JSON header        (header length) bytes │
├──────────────────────────────────────────┤
│ Packet 1           variable              │
│ Packet 2           variable              │
│ ...                                      │
│ EOF marker         13 bytes              │
└──────────────────────────────────────────┘
```

## File Header

| Offset | Size | Type | Field | Description |
|--------|------|------|-------|-------------|
| 0 | 16 | bytes | Magic | `DBBAT_DUMP\x00\x00\x00\x00\x00\x00` |
| 16 | 2 | uint16 BE | Version | `2` |
| 18 | 4 | uint32 BE | Header length | Byte length of the JSON header that follows |
| 22 | N | UTF-8 | JSON header | Session metadata (see below) |

The magic string is always 16 bytes. The first 10 bytes (`DBBAT_DUMP`) are the significant portion; the remaining 6 bytes are null padding reserved for future use.

### Version History

| Version | Description |
|---------|-------------|
| 1 | Original format with fixed binary header (Oracle-specific fields) |
| 2 | JSON metadata header, protocol-agnostic |

## JSON Header

The JSON header is a single JSON object encoded as UTF-8 with no trailing null. Its byte length is given by the header length field.

### Required Fields

| Field | Type | Description |
|-------|------|-------------|
| `session_id` | string | UUID v4 identifying the proxy session |
| `protocol` | string | Protocol identifier (see below) |
| `start_time` | string | RFC 3339 timestamp with nanosecond precision |
| `connection` | object | Protocol-specific connection metadata |

### Protocol Identifiers

| Value | Protocol |
|-------|----------|
| `oracle` | Oracle TNS/TTC |
| `postgresql` | PostgreSQL wire protocol |

New protocols add a new identifier. No format version bump needed.

### Connection Object

The `connection` object contains protocol-specific fields. Readers must ignore unknown keys.

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

Additional top-level keys or connection keys may be added at any time. Readers must tolerate and ignore unknown keys. Examples of future additions:

- `proxy_version` — dbbat version that wrote the file
- `connection.tls` — whether the upstream connection used TLS
- `connection.client_addr` — client source address
- `labels` — user-defined key/value tags

## Packet Stream

Immediately after the JSON header, the packet stream begins. Each packet is a fixed-size frame followed by variable-length data.

### Packet Frame (13 bytes)

| Offset | Size | Type | Field | Description |
|--------|------|------|-------|-------------|
| 0 | 8 | int64 BE | Relative time | Nanoseconds since `start_time` |
| 8 | 1 | uint8 | Direction | `0x00` = client → server, `0x01` = server → client |
| 9 | 4 | uint32 BE | Data length | Byte length of packet data that follows |

Followed by `data_length` bytes of raw packet data (opaque to the format — interpretation depends on the protocol).

### EOF Marker

The last entry in the packet stream signals end-of-file:

| Field | Value |
|-------|-------|
| Relative time | Nanoseconds since `start_time` (capture duration) |
| Direction | `0xFF` |
| Data length | `0x00000000` |

The EOF marker is 13 bytes, identical in structure to a normal packet frame. Readers stop when they encounter `direction = 0xFF`.

### Maximum Packet Size

Data length is a uint32, giving a theoretical maximum of ~4 GB per packet. In practice, database wire protocols use much smaller packets (Oracle TNS: up to 64 KB typically, PostgreSQL: up to 1 GB messages). Implementations may enforce a lower limit via configuration (`DBB_ORACLE_DUMP_MAX_SIZE`, etc.).

## File Size Enforcement

Writers may enforce a maximum file size. When the next packet would exceed the limit, the writer silently skips it and writes the EOF marker on close. The `max_size` limit applies to the entire file (header + packets).

## Reading Algorithm

```
1. Read 16 bytes → verify magic
2. Read 2 bytes  → format version (must be 2)
3. Read 4 bytes  → header_length
4. Read header_length bytes → parse as JSON
5. Loop:
   a. Read 13 bytes → packet frame
   b. If direction == 0xFF → EOF, stop
   c. Read data_length bytes → packet data
   d. Yield (relative_time, direction, data)
```

## Backward Compatibility

Version 2 files are **not** backward-compatible with version 1 readers. The magic is identical, so readers must check the version field:

- Version 1: fixed binary header (legacy, Oracle-only)
- Version 2: JSON header (this spec)

Implementations should support reading both versions during the migration period.
