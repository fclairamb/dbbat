# Oracle Proxy — TNS/TTC Protocol Notes

Findings from implementing the Oracle proxy in dbbat. This documents the wire protocol behavior observed with real Oracle 19c databases, covering TNS packet framing, TTC function codes, SQL extraction, and result row capture.

## TNS Packet Format

Every Oracle network message is wrapped in a TNS (Transparent Network Substrate) packet.

### Header (8 bytes)

```
Offset  Size  Field
0       2     Packet length (big-endian)
2       2     Packet checksum (usually 0x0000)
4       1     Packet type
5       1     Flags
6       2     Header checksum (usually 0x0000)
```

### Packet Types

| Code | Name | Direction |
|------|------|-----------|
| 1 | Connect | C→S |
| 2 | Accept | S→C |
| 3 | Refuse | S→C |
| 4 | Redirect | S→C |
| 5 | Marker | Bidir |
| 6 | Data | Bidir |
| 11 | Resend | S→C |
| 12 | Control | Bidir |

### TNS Version >= 315 (v315+)

Modern Oracle clients (oracledb Python thin, JDBC thin, go-ora) negotiate TNS version 315+. This changes the packet framing:

**Connect packet**: Uses 2-byte length in the header, but the connect data is appended AFTER the initial header+metadata. The connect data offset (at payload bytes 18-19) indicates where it starts relative to the full packet (including 8-byte header).

**All other packets after Accept**: Use a **4-byte length** at bytes 0-3. The 2-byte length field reads as `0x0000`. The proxy must detect this and read the length as `binary.BigEndian.Uint32(header[0:4])`.

```
v315+ Data packet header:
  Bytes 0-3: packet length (uint32 BE) — NOT 2 bytes!
  Byte  4:   packet type (6 = Data)
  Bytes 5-7: flags/checksum
```

This is the single most important thing to get right. If you read the length as 2 bytes, you get 0, and the packet appears empty.

### Connect Data Offset

The connect descriptor offset at payload bytes 18-19 is from the **start of the full TNS packet** (including the 8-byte header). When indexing into the payload (which starts after the header), subtract 8.

However, for v315+ clients with extended connect data (appended after the initial packet), the raw offset may work directly because the extended data is appended to the payload array.

The safe approach: try both `payload[offset]` and `payload[offset - 8]`, and validate which one contains `(DESCRIPTION` or `SERVICE_NAME`.

## TTC Protocol (Two-Task Common)

TTC messages are carried inside TNS Data packets. The layout:

```
TNS Data packet payload:
  Bytes 0-1: Data flags (usually 0x0000)
  Byte  2:   TTC function code
  Byte  3+:  Function-specific payload
```

### Function Codes (v315+)

In modern Oracle, function code `0x03` is a generic "piggyback" that carries sub-operations identified by byte 1 of the TTC payload:

| TTC func | Sub-op (byte 1) | Purpose |
|----------|-----------------|---------|
| 0x01 | — | Set Protocol (session init) |
| 0x02 | — | Set Data Types (session init) |
| 0x03 | 0x5e | **Execute with SQL** (OALL8 equivalent) |
| 0x03 | 0x76 | AUTH Phase 1 |
| 0x03 | 0x73 | AUTH Phase 2 |
| 0x03 | 0x09 | Close cursor |
| 0x08 | — | Server response (legacy) |
| 0x09 | — | Close/marker |
| 0x10 | — | **Query result with row data** |
| 0x11 | — | Fetch rows |
| 0xde | — | JDBC initial negotiation |

### SQL Extraction

SQL text is inside piggyback execute messages (func=0x03, sub=0x5e). The SQL is length-prefixed, but its exact offset varies by client driver:

| Client | SQL offset in TTC payload |
|--------|--------------------------|
| Python oracledb thin | ~50 |
| JDBC thin (ojdbc) | ~54 |
| Go go-ora | varies |

The robust approach: scan offsets 40-70 for a `decodeVarLen` + readable SQL text, then validate with `looksLikeSQL()` (checks for SQL keyword prefix). As a fallback, scan the entire payload for SQL keywords (`SELECT`, `INSERT`, etc.) and extract until end of printable ASCII.

### Query Results

#### First response (func=0x10, QueryResult)

Contains column definitions and the first batch of rows:

```
[func=0x10] [cursor metadata ~23 bytes]
[column definitions: length-prefixed uppercase names]
[0x06 0x22 marker + descriptor]
[0x07 separator]
[row 1: col1_len col1_data col2_len col2_data ...]
[0x07 separator]
[row 2: ...]
[0x08 0x01 0x06 footer — end of rows in this packet]
```

Column names are scanned in the area **before** the `0x06 0x22` marker. They must be uppercase ASCII identifiers with minimum 2 characters (to avoid false positives from random bytes).

#### Continuation packets (func=0x06)

For large result sets, additional rows arrive in separate Data packets with func=0x06:

```
[func=0x06] [header ~13 bytes]
[row data: same length-prefixed format]
[0x07 separators between rows]
[0x08 footer or ORA-01403 end marker]
```

The `ORA-01403: no data found` string appears in the **last** continuation packet and signals end of the result set. This is not an error — it's Oracle's normal end-of-data indicator.

#### Row data format

Each column value is length-prefixed:
- `0x00` = NULL
- `0x01-0xFD` = length, followed by that many bytes of value data
- Values can be strings (ASCII), Oracle NUMBER, Oracle DATE, or other types

### Oracle NUMBER Encoding

Oracle NUMBER is a variable-length format:

```
Byte 0:     Exponent (value - 193 = power of 100)
Byte 1..N:  Mantissa digits (each byte - 1 = two-digit number 00-99)
```

Examples:
- `c1 02` → exponent=0, digit=1 → **1**
- `c1 2b` → exponent=0, digit=42 → **42**
- `c2 03 15` → exponent=1, digits=2,20 → **220**
- `c2 16 44` → exponent=1, digits=21,67 → **2167**

Special case: `0x80` alone = **0**

### Oracle DATE Encoding

7 bytes:

```
Byte 0: century (value - 100, so 120 = 20th century → 2000s)
Byte 1: year   (value - 100, so 126 = year 26 → 2026)
Byte 2: month  (1-12)
Byte 3: day    (1-31)
Byte 4: hour   (value - 1, so 1 = 00:00)
Byte 5: minute (value - 1)
Byte 6: second (value - 1)
```

Example: `78 7e 04 04 13 2f 1c` → 2026-04-04 18:46:27

## Connection Flow

```
Client                          DBBat Proxy                     Oracle
  │                                │                               │
  │  TNS Connect (service_name)    │                               │
  │───────────────────────────────>│                               │
  │                                │  Look up database by          │
  │                                │  service_name or db name      │
  │                                │                               │
  │                                │  TNS Connect (forwarded)      │
  │                                │──────────────────────────────>│
  │                                │  TNS Resend                   │
  │                                │<──────────────────────────────│
  │  TNS Connect (retry)           │  TNS Connect (forwarded)      │
  │                                │──────────────────────────────>│
  │                                │  TNS Accept                   │
  │  TNS Accept                    │<──────────────────────────────│
  │<───────────────────────────────│                               │
  │                                │                               │
  │  TTC Set Protocol              │  (relayed transparently)      │
  │  TTC Set Data Types            │                               │
  │  TTC AUTH Phase 1 + 2          │                               │
  │<──────────────────────────────>│<─────────────────────────────>│
  │                                │                               │
  │  TTC Execute (SQL)             │  Intercept: extract SQL       │
  │───────────────────────────────>│  Log query, forward           │
  │                                │──────────────────────────────>│
  │                                │  Response + rows              │
  │  Response + rows               │  Intercept: capture rows      │
  │<───────────────────────────────│<──────────────────────────────│
```

The proxy is fully transparent — it forwards raw TNS packets without modification. SQL extraction and row capture happen by inspecting copies of the data, never altering the traffic.

## Known Limitations

- **TTC auth interception disabled**: The proxy doesn't extract the Oracle username from TTC AUTH messages. It uses the first user with an active grant for connection tracking.
- **Row capture is best-effort**: The TTC binary format varies across Oracle client versions. Some clients/query types may produce partial or no row capture. SQL text extraction works reliably across all tested clients.
- **No result capture for DML**: INSERT/UPDATE/DELETE statements are logged (SQL text + duration) but row counts are not captured from v315+ responses.
- **TIMESTAMP with timezone**: Complex Oracle temporal types may appear as hex in captured results. Simple DATE works correctly.
- **Multi-packet rows**: Large result sets spanning multiple TNS Data packets capture rows from the first response and continuation packets, but some middle packets may be missed depending on their format.

## Testing

The Oracle proxy has been tested with:

| Client | Library | Status |
|--------|---------|--------|
| Python | oracledb (thin mode) | SQL + rows work |
| Java | ojdbc11 (JDBC thin) | SQL works, row capture partial |
| Go | go-ora | SQL works |
| DBeaver | JDBC thin via ojdbc | Connects, SQL logged, row capture partial |

For debugging, enable `DBB_LOG_LEVEL=debug` to see TTC function codes and SQL extraction details.
