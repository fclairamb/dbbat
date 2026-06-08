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
| 0x04 | — | **OER — error/status** (carries DML row count or ORA error) |
| 0x08 | — | Server response (carries an embedded OER on v315+) |
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

Rows use **column-level compression**: a row sends values only for the columns
that changed; unchanged columns keep their previous value. The marker between
two rows says which columns the next row carries:
- `0x07` — bare separator; the next row carries **all** columns.
- `0x15 [flag] [count] [bitmask…] 0x07` — descriptor; `bitmask` bit *i* set means
  column *i* is present in the next row. The bitmask spans `ceil(numCols/8)`
  bytes and is parsed structurally — **not** by scanning to the `0x07`
  terminator, because a bitmask byte can itself be `0x07` (columns 0,1,2 → mask
  `0x07`); scanning would truncate the descriptor and corrupt the next row.

The same stream — both the func `0x10` QueryResult row area and func `0x06`
continuation packets — is decoded by `parseRowStream` in `ttc_decode.go`.
Verified against `testdata/go_ora_compressed.dbbat-dump`
(`TestDumpReplay_CompressedRows`): runs of a repeated column, NULLs, and the
all-columns-change boundary.

#### DML status (OER, func=0x04)

INSERT/UPDATE/DELETE don't return rows — their outcome is an OER status block.
On v315+ it is **embedded inside the execute Response** (func=0x08); a failed
statement (e.g. dropping a missing table) instead arrives as a **standalone**
func=0x04 packet after a marker exchange. The block begins at a `0x04` marker
followed by TTC compressed integers:

```
[0x04] [callStatus] [seqNum] [curRowNumber] [errNum] [arrayElemWErr] [arrayElemErrNo] [cursorID] ...
```

- `curRowNumber` is the affected-row count (rows processed; `0` for DDL).
- `errNum` is `0` on success, `1403` for end-of-data (ORA-01403, not an error),
  or the `ORA-NNNNN` code on failure — followed later by the CLR-prefixed
  `ORA-...` message text.
- `callStatus` always has the end-of-call bit `0x010000` set on a real OER,
  which `decodeOERAt` uses to reject stray `0x04` bytes inside the preceding
  return-parameter block. See `ttc_oer.go` and `findOERInResponse`.

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

### Oracle TIMESTAMP Encoding

TIMESTAMP extends DATE with fractional seconds; TIMESTAMP WITH TIME ZONE adds a
zone. The instant is stored in **UTC**.

```
Bytes 0-6:  DATE portion (UTC wall clock, same layout as above)
Bytes 7-10: fractional seconds — nanoseconds, big-endian uint32
Bytes 11-12 (WITH TIME ZONE only):
  If byte 11 high bit (0x80) is clear → numeric offset:
    tz hours   = byte 11 - 20
    tz minutes = byte 12 - 60   (both go negative for negative offsets)
  If byte 11 high bit is set → named-region id (not resolved to an offset here)
```

- 11 bytes → TIMESTAMP / TIMESTAMP WITH LOCAL TIME ZONE (rendered as UTC wall clock).
- 13 bytes → TIMESTAMP WITH TIME ZONE (rendered as the original local wall clock
  = stored UTC + offset, with a `+HH:MM` suffix).

Example: `78 7e 05 18 08 05 39 2f 07 5e 20 19 5a` → stored UTC `2026-05-24 07:04:56.789012`,
offset `25-20=+5h` / `90-60=+30m` → **`2026-05-24 12:34:56.789012 +05:30`**.

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

- **Single O5LOGON key per user**: The Oracle username from TTC AUTH Phase 1 maps to the dbbat user (lowercased) for grant checks and connection tracking, but only that user's first verifier-bearing API key can authenticate — see "Per-user O5LOGON key" below.
- **Row capture is best-effort**: The TTC binary format varies across Oracle client versions. Some clients/query types may produce partial or no row capture. SQL text extraction works reliably across all tested clients.
- **Undetectable column names**: The column count is read from the describe header (`describeColumnCount`: `[0x10][size][…][maxRowSize][colCount]`), so queries whose column names the scanner can't detect — single-char aliases (`SELECT level AS n`) or unnamed expressions (`SELECT 1`, `SELECT level*10`) — still capture all rows. Those undetectable columns are stored under synthetic `COLn` names (the value is correct; only the label is generic), and when *some* names are detected the synthetic ones are appended at the end, so labels can misalign with positions. Proper per-column names would require parsing the describe column-definition records.
- **DML row counts**: INSERT/UPDATE/DELETE affected-row counts are captured from the v315+ OER status block (TTC func `0x04`, embedded in the execute Response) and stored as `rows_affected`. Failed statements record the ORA error text. See `ttc_oer.go`.
- **Temporal types**: DATE, TIMESTAMP, and TIMESTAMP WITH TIME ZONE decode in captured results (the tz form renders the original local wall clock plus its numeric offset). Named-region time zones fall back to the stored UTC wall clock without an offset suffix.
- **Large result sets**: The QueryResult (func `0x10`) row area and continuation packets (func `0x06`) share one decoder (`parseRowStream`) that walks the full compressed row stream — length-prefixed values plus the `0x15 [flag] [count] [bitmask] 0x07` column-compression descriptors between rows. A 400-row single-packet result is captured end-to-end against a live-Oracle ground-truth fixture (`testdata/go_ora_largeresult.dbbat-dump`, `TestDumpReplay_LargeResultRows`). Multi-TNS-packet (small-SDU/JDBC) result sets reuse the same decoder via the continuation path; their per-row correctness is not yet ground-truth-verified.

## Testing

The Oracle proxy has been tested with:

| Client | Library | Status |
|--------|---------|--------|
| Go | go-ora | SQL + rows work end-to-end |
| Python | oracledb (thin mode) | SQL works end-to-end (verified through dbbat → Oracle 19c) |
| Java | ojdbc11 (JDBC thin) | SQL works, row capture partial (older tests) |
| DBeaver | JDBC thin via ojdbc | Connects, SQL logged, row capture partial (older tests) |
| SQLcl | JDBC thin (Oracle 23c+) | SQL works end-to-end |
| sqlplus | OCI (Oracle 23c) | Not yet supported — see below |

For debugging, enable `DBB_LOG_LEVEL=debug` to see TTC function codes and SQL extraction details.

### Authentication path

The proxy negotiates TNS Connect / Accept / Set Protocol / Set Data Types in a transparent
relay to the upstream Oracle, then takes over once the client sends `AUTH Phase 1`. Two
things happen at that boundary:

1. **The relay-phase upstream socket is kept open** through the AUTH boundary. After dbbat
   completes O5LOGON with the client (using the API key as the Oracle password), it runs
   an O5LOGON CLIENT — the inverse role — against the *same* upstream socket using stored
   database credentials. Reusing the socket keeps the TTC compile-time capability levels
   aligned end-to-end. Closing it and opening a fresh go-ora session would shift the
   upstream's view of caps; caps-rich drivers like SQLcl JDBC thin 23.x would then have
   their OALL8 messages parsed at the wrong level and Oracle would respond with two TNS
   Marker (interrupt) packets followed by `ORA-03120: two-task conversion routine: integer
   overflow`.
2. **The relay strips the `customHash` flag** (`caps[4]&0x20`) from the upstream's Set
   Protocol response **as it is forwarded to the client**. Without that strip, modern
   clients switch to a PBKDF2 combined-key derivation that dbbat's O5LOGON server doesn't
   implement, and `AUTH_PASSWORD` decrypts to garbage. The bit is preserved on the
   server-as-client AUTH path (recorded into `session.upstreamCustomHash` before stripping),
   so dbbat's outgoing AUTH messages use the modern PBKDF2 / verifier-18453 derivation
   that real 19c expects.

The upstream-as-client path supports both the legacy SHA-1 / verifier 6949 derivation and
the modern HMAC-SHA512 / verifier 18453 path with `customHash` enabled. It mirrors the
algorithms in `go-ora/v2/auth_object.go` but does not depend on go-ora at runtime — it
runs against the raw `net.Conn` returned by the pre-auth relay.

Once upstream auth completes, dbbat forwards the **real** upstream AUTH OK packet to the
client (not a static capture), so all session-specific fields — instance metadata,
`AUTH_SESSION_ID`, `AUTH_SC_*`, etc. — match the live session. The one field it rewrites is
`AUTH_SVR_RESPONSE` (`patchAuthSvrResponse`): the upstream encrypts it with the proxy↔upstream
combined key, but modern clients decrypt it with the client↔proxy combined key to confirm the
server holds the negotiated session key. dbbat re-encrypts it in place under the client's key.
Without this, python-oracledb thin rejected the AUTH OK with `DPY-4035` and JDBC thin / SQLcl
with `ORA-17401`. go-ora ignores the field, which is why the earlier static-capture path
worked for it while silently breaking everyone else. The static `capturedAuthOKResponse`
remains only as a fallback when no upstream packet was captured.

### Known client limitations

- **sqlplus 23c** initiates Oracle Native Services (NS) negotiation via OOB break/reset
  markers after the AUTH challenge. dbbat doesn't implement the NS protocol layer, so
  sqlplus errors with `ORA-12630`.

For now, `go-ora`, python-oracledb thin, and SQLcl 23c+ work end-to-end; sqlplus (OCI)
reaches AUTH but not query execution.

### Per-user O5LOGON key

dbbat picks the connecting user's first API key with an O5LOGON verifier when generating the AUTH challenge — see the `O5LOGON verifier loaded` info log. That specific key (and only that one) is the password your Oracle client must supply: the salt sent in the challenge is bound to it, so any other API key fails to decrypt. Multi-key support is not yet implemented (it would require all of a user's keys to share one salt, since the challenge can only carry one).
