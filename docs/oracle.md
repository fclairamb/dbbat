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

Oracle NUMBER is a variable-length, sign-and-magnitude, base-100 format:

```
Byte 0:     Exponent + sign. High bit set = positive; base-100 exponent =
            (byte & 0x7f) - 65 (positive) or ((byte ^ 0xff) & 0x7f) - 65 (negative).
Byte 1..N:  Base-100 mantissa digits. Positive: digit = byte - 1 (00-99).
            Negative: digit = 101 - byte, with a trailing 0x66 terminator.
```

The value is `sign × mantissa × 100^(exp100 - n + 1)`; `formatOracleNumber` lays
the digits out two decimal places each and places the point accordingly, so
integers **and fractionals of either sign** decode exactly. Examples:
- `c1 02` → **1**
- `c1 2b` → **42**
- `c1 04 0f` → exp100=0, digits 3,14 → **3.14**
- `c0 33` → exp100=-1, digit 50 → **0.5**
- `3e 3b 66` → **-42**

Special case: `0x80` alone = **0**.

Cross-checked against go-ora's reference decoder in `TestDecodeOracleNumberToString_Goora`
and verified end-to-end against `testdata/go_ora_numbers.dbbat-dump`
(`TestDumpReplay_Numbers`).

When the column type is known (from the describe records — see "Column names"),
NUMBER values are decoded by type via `formatOracleNumber`, so negative NUMBERs
decode correctly. Without a type — continuation packets, or a server layout the
describe parser can't read — the proxy falls back to `decodeOracleRawValue`,
which tries ASCII first; a negative NUMBER whose bytes all fall in the printable
ASCII range (e.g. `-42`) is then captured as text.

`BINARY_FLOAT` (4 bytes) and `BINARY_DOUBLE` (8 bytes) are stored in a sortable
form — the sign bit is flipped for positive values and every bit is inverted for
negative values — so the raw bytes order numerically. `decodeOracleBinaryFloatString`
undoes that transform before reading the IEEE-754 value; these need the column
type (4/8 raw bytes are otherwise ambiguous).

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
zone. The 7-byte prefix holds either the UTC instant or the local wall clock,
selected by byte 11's `0x40` flag (see below).

```
Bytes 0-6:  DATE portion (same layout as above)
Bytes 7-10: fractional seconds — nanoseconds, big-endian uint32
Bytes 11-12 (WITH TIME ZONE only):
  If byte 11 high bit (0x80) is set → named-region id (not resolved to an offset here)
  Else → numeric offset (only the low 6 bits of byte 11 hold the hour):
    tz hours   = (byte 11 & 0x3f) - 20
    tz minutes =  byte 12 - 60          (both go negative for negative offsets)
    byte 11 bit 0x40 = "time in zone" flag:
      set   → bytes 0-6 are already the LOCAL wall clock (no shift)
      clear → bytes 0-6 are UTC; shift into the offset zone to get local time
```

- 11 bytes → TIMESTAMP / TIMESTAMP WITH LOCAL TIME ZONE (rendered as UTC wall clock).
- 13 bytes → TIMESTAMP WITH TIME ZONE (rendered as the local wall clock with a
  `+HH:MM` suffix).

Examples (both render to a `+05:30` local clock):
- 19c, flag clear: `78 7e 05 18 08 05 39 2f 07 5e 20 19 5a` → byte 11 `0x19`, prefix is
  UTC `07:04:56`, shift `+5h30m` → **`2026-05-24 12:34:56.789012 +05:30`**.
- Free 23ai, flag set: `78 7c 03 0f 0f 1f 2e 07 5b ca 00 59 5a` → byte 11 `0x59`
  (`0x59&0x3f=0x19=25 → +5h`, `0x40` set), prefix `14:30:45` is already local →
  **`2024-03-15 14:30:45.123456 +05:30`**.

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
- **Column names**: Real column names come from the describe column-definition records (`parseColumnDescribes` in `describe.go`), so single-char aliases (`SELECT level AS n`) and unnamed expressions (`SELECT count(*)`) get their true names and positions. Only genuinely unnamed expression columns fall back to a synthetic `COLn` label. If the records don't parse on some server layout, decoding falls back to heuristic name-scanning plus describe-header count padding, so the column count (and row framing) stays correct.
- **DML row counts**: INSERT/UPDATE/DELETE affected-row counts are captured from the v315+ OER status block (TTC func `0x04`, embedded in the execute Response) and stored as `rows_affected`. Failed statements record the ORA error text. See `ttc_oer.go`.
- **Bind values (parameterized queries)**: Bind values are captured from both the legacy `OALL8` execute path (`decodeBindValues`) and the v315+ **piggyback exec** path that modern clients use (`extractPiggybackBinds`, func `0x03` sub `0x5e`). The piggyback binds sit length-prefixed at the tail of the message; they're located as the suffix that parses as exactly as many values as there are distinct bind placeholders in the SQL, and each is decoded by content via `decodeOracleRawValue` (so a NUMBER bind like `42` renders as `42`, not hex). Verified against `testdata/go_ora_binds.dbbat-dump` (`TestDumpReplay_Binds`). Captured binds are now persisted to `queries.parameters` (`formatOracleBinds` wired into `persistQueryRecord` and `completeQuery`), so the API (`GET /api/v1/queries/:uid`) and the UI Parameters card report them. Not yet handled: binds over ~253 bytes (extended length encoding) and full type-aware decoding from the bind-definition records.
- **Temporal types**: DATE, TIMESTAMP, and TIMESTAMP WITH TIME ZONE decode in captured results, verified end-to-end against `testdata/go_ora_temporal.dbbat-dump` (`TestDumpReplay_Temporal`). The tz form renders the local wall clock plus its numeric offset, honouring byte 11's `0x40` "time in zone" flag (prefix stored as local vs UTC). Named-region time zones fall back to the stored wall clock without an offset suffix.
- **Large result sets**: The QueryResult (func `0x10`) row area and continuation packets (func `0x06`) share one decoder (`parseRowStream`) that walks the full compressed row stream — length-prefixed values plus the `0x15 [flag] [count] [bitmask] 0x07` column-compression descriptors between rows. A 400-row single-packet result is captured end-to-end against a live-Oracle ground-truth fixture (`testdata/go_ora_largeresult.dbbat-dump`, `TestDumpReplay_LargeResultRows`). Multi-TNS-packet (small-SDU/JDBC) result sets reuse the same decoder via the continuation path; their per-row correctness is not yet ground-truth-verified.

## Testing

The Oracle proxy has been tested with:

| Client | Library | Status |
|--------|---------|--------|
| Go | go-ora | SQL + rows + **bind values** end-to-end (verified vs Oracle 23ai Free) |
| Python | oracledb (thin mode) | SQL works vs Oracle 19c; **fails at AUTH vs Oracle 23ai** — see "Modern thin clients" below |
| Java | ojdbc11 (JDBC thin) | SQL works, row capture partial (older tests) |
| DBeaver | JDBC thin via ojdbc | Connects, SQL logged, row capture partial (older tests) |
| SQLcl | JDBC thin (Oracle 23c+) | SQL works vs 19c; **fails at AUTH vs Oracle 23ai** (`ORA-03113 … Get the session key`) |
| sqlplus | OCI (Oracle 23c) | Fails at AUTH vs Oracle 23ai |

For debugging, enable `DBB_LOG_LEVEL=debug` to see TTC function codes and SQL extraction details.

### Pre-auth relay (Oracle 23ai)

The pre-auth negotiation is **not** strict request/response: a single client packet can
elicit several upstream packets, and 23ai injects Control/Marker (OOB break/reset) packets
mid-negotiation. The relay (`relayPreAuthNegotiation`) therefore runs a **concurrent
bidirectional pump** (upstream→client in a goroutine; client→upstream in the main loop)
until it sees AUTH Phase 1 — an earlier lockstep "one upstream read per client packet"
relay **deadlocked** the moment the counts diverged, hanging *every* client on 23ai.

Modern clients also **pipeline** the login into one FAST_AUTH packet (TNS message type
`0x22`): `[0x22][ver][convChars][0]` + Set Protocol + `[charset:2][csFlag:1][ncharset:2]
[ttcVer:1]` + Set Data Types + AUTH Phase 1, written back-to-back. `splitBundledAuthPhase1`
de-pipelines this — replaying Set Protocol / Set Data Types to the upstream as classic
standalone messages and carving out the embedded AUTH Phase 1 for terminated O5LOGON.
`stripAcceptModernAuthFlags` also clears `FAST_AUTH` (`0x10000000`) and
`HAS_END_OF_RESPONSE` (`0x02000000`) from the Accept's 4-byte connect-flags (offset 41,
v315+) so clients fall back to the classic flow dbbat terminates.

The Set Protocol response capability array is framed `[numCaps][06 01 01 01][caps…]`, where
`numCaps` varies by server version (`0x2a` on 19c, `0x36` on 23ai) — `observeCustomHashFlag`
anchors on the stable `06 01 01 01` prefix (not a version literal) and reads caps[0]&0x20.
`stripCustomHashFromSetProto` clears that bit toward the client so it negotiates the
verifier-6949 O5LOGON dbbat issues, while dbbat still uses customHash upstream.

### Client compatibility on Oracle 23ai

Verified end-to-end (authenticate + query + observability capture) against Oracle 23ai
(`23.26`) through the cluster proxy from the Windows host:

| Client | Protocol | Status | Notes |
|--------|----------|--------|-------|
| go-ora | thin | ✅ works | accepts 6949 or 18453 |
| python-oracledb thin | thin | ✅ works | FAST_AUTH de-pipelined; verifier 18453 |
| SQLcl 26.1.2 (ojdbc) | thin | ✅ works | classic O5LOGON; verifier 18453 |
| sqlplus / OCI instant client | thick | ✅ works | auth + query work via the **wide** (4-byte LE) TTC encoding, with **no dependency on OOB/`DISABLE_OOB`** — verified locally against Oracle 23ai and through an OOB-stripping TCP relay (a NodePort/NLB stand-in). See "OCI wide encoding" and "OCI break/reset before AUTH Phase 2" below |

Each API key now stores **both** verifiers (`api_keys.o5logon_verifier` 6949 and
`o5logon_verifier_18453` + `o5logon_salt_18453`). When the upstream's Set Protocol
response advertises `customHash` (23ai), `authenticateClient` switches the O5LOGON server
to the 18453 (PBKDF2 / HMAC-SHA512) challenge — `AUTH_PBKDF2_CSK_SALT`,
`AUTH_PBKDF2_VGEN_COUNT`, `AUTH_PBKDF2_SDER_COUNT`, `AUTH_GLOBALLY_UNIQUE_DBID`,
`AUTH_SESSKEY` flag 0 — which modern thin clients require. Legacy go-ora reads the
verifier type from the challenge's `AUTH_VFR_DATA` flag and uses 6949.

#### Proxy-mode robustness (must never crash on a malformed packet)

Query/response interception in proxy mode is **best-effort observability**: it decodes a
copy for logging but forwards every packet byte-exact regardless. A decode error must
never break the connection — and a *panic* must never take down the whole process. Two
guards enforce this:

- `dlc()` (`describe.go`) rejects a negative length. SQLcl/ojdbc negotiates a high
  TTCVersion, so the server's column-describe records carry the modern domain/annotation
  layout the parser misaligns on; a NUMBER scale's `-127` sentinel was then read as a
  length, producing `data[:-127]` — a panic that crashed the **entire** dbbat process for
  *all* connections. Now it returns nil and `parseColumnDescribes` bails out.
- `interceptClientMessage` / `interceptUpstreamMessage` each `recover()` from any panic in
  the decode path and forward the packet unchanged.

See `sqlcl_regression_test.go` for both guards (real SQLcl 26.1.2 fixtures).

#### SQLcl/ojdbc exec SQL capture

SQLcl sends its statements via the `func=0x11` JDBC exec, where the SQL follows a run of
zero bytes (no length prefix immediately before it). `findSQLInPayload` locates it by a
**case-insensitive** keyword scan (SQLcl lowercases its SQL).

#### SQLcl/ojdbc result capture — modern column-describe + 8-byte row values

SQLcl/ojdbc negotiates a high TTCVersion, so the func=0x10 QueryResult's per-column
describe records carry the modern (TTCVersion ≥ 17/20) trailing layout — data-use-case
domain schema/name DLCs, an annotations block, and three further ints — that the classic
parser misaligns on. `parseColumnDescribe(c, modern)` consumes those when needed and
`parseColumnDescribes` auto-detects the layout: it tries the classic record first (so thin
clients never regress) and retries the modern one only when the classic parse misaligns
(an unknown TTC type or a record running off the end). go-ora v2.9.0 is the field-order
reference but is stale for 23ai — the three extra trailing ints were recovered empirically
from a real SQLcl describe (`sqlcl_regression_test.go`).

Once columns parse, rows are located independently by `scanRowValues`. A second, latent bug
surfaced there: `parseRowStream` treated a leading `0x08` as the end-of-rows footer, but
`0x08` is also a valid column-value length (an 8-byte first value such as the string
`sqlcl-ok`), so such rows vanished. The footer is the 3-byte sequence `08 01 06`; matching
the full footer fixes it. SQLcl SELECT results (columns + rows, single- and multi-row) are
now captured like any other client.

#### OCI wide (4-byte little-endian) TTC encoding

OCI clients (sqlplus / instant client) negotiate a different TTC integer encoding than thin
clients: the AUTH key/value **lengths and flags are fixed 4-byte little-endian integers**,
not the compressed length-prefixed form go-ora / python-oracledb thin / JDBC thin use. dbbat
detects the client's encoding from its AUTH Phase 1 (`payloadUsesWideKVEncoding`: a 4-byte LE
key length — three high zero bytes — precedes the 1-byte CLR length, which the compressed
form `01 0d` never produces) and mirrors it across the whole terminated-auth path:

- the **challenge** (`buildAuthChallenge` / `ttcKeyValWide` / `buildAuthChallengeEndMarker`):
  data flags `20 00`, a 2-byte LE dictionary count, 4-byte LE key/val lengths and flags, and
  a 153-byte wide end-of-call summary. Verified byte-for-byte against a real Oracle 23ai
  classic 18453 challenge to an OCI client.
- **Phase 2 parsing** (`parseAuthPhase2` → `findKVByKeyBytesWide`): value lengths are read as
  4-byte LE.
- the **upstream rewrite** (`rewriteAuthPhase1UsernameAnchored`, `replaceAuthKVValueWide`):
  the user_id_len is a 4-byte LE field tens of bytes ahead of the username; AUTH_SESSKEY /
  AUTH_PASSWORD values are spliced preserving the (sometimes buffer-sized) 4-byte key length.
- the **upstream challenge parse** (`parseAuthKVDictionary` / `readAuthKVPairWide`): the
  upstream negotiated wide with the client's relayed caps, so its challenge is wide too.

With this, sqlplus authenticates and runs queries end-to-end (verified locally against
Oracle 23ai, `DISABLE_OOB` unset); the SQL is captured like any other client.

Four more OCI-only fixes complete the wide path (all captured/verified against the macOS
Oracle Instant Client 23.3 and the DB-bundled 23.26 OCI client — the two flavors differ on
the wire, so both are covered by fixtures in `oci_instantclient_test.go`):

- **Client challenge end-of-call summary** (`clientChallengeTrailer`, `session.go`): the
  summary appended after the AUTH challenge KV dictionary is **caps-conditioned** — 80
  bytes for instantclient 23.3, 153 for the 23.26 bundled client. A fixed-width capture
  only fits the client it came from; any other client leaves unread bytes in its TTC read
  buffer, treats the next stale byte as a message code, and aborts the AUTH call with an
  inline break/reset marker exchange — the "sqlplus stalls before AUTH Phase 2" symptom.
  dbbat therefore runs upstream AUTH Phase 1 first (`beginUpstreamAuth`, before it
  challenges the client) and reuses the **live upstream challenge's summary bytes**, which
  the real server sized for these exact caps.
- **Phase 1 user-len locator** (`findUserIDLenPos`, `phase1_forward.go`): the wide preamble
  encodes `user_id_len` as a 4-byte LE field after the first `fe…`-pointer run — sometimes
  as a 3× UTF-8 max-expansion buffer size. It must be found by anchoring on that pointer
  run, **never** by scanning backward for a dword equal to the old length: the KV pair
  count is also a small 4-byte LE integer between pointer runs, and a backward scan
  corrupts it whenever `len(username) == numPairs` (the 5-char `admin` collides with the
  OCI Phase-1 pair count of 5), after which the upstream waits forever for a pair that
  never arrives and AUTH hangs.
- **Phase 2 value length convention** (`replaceAuthKVValueWide`, `phase2_forward.go`): when
  splicing AUTH_SESSKEY / AUTH_PASSWORD / AUTH_PBKDF2_SPEEDY_KEY, the 4-byte LE value
  length must mirror the client's convention. instantclient 23.3 sends every value length
  as a 3× buffer size; a spliced plain length draws `ORA-28041` ("authentication protocol
  internal error") from a 23ai parsing at that client's caps.
- **AUTH OK reassembly + re-fragmentation** (`readUpstreamAuthMessages` / `reframeAuthOK`,
  `upstream_auth_client.go`): Oracle 23ai splits an OCI AUTH OK across two Data packets
  (observed 1967+557 bytes) with the `AUTH_SVR_RESPONSE` hex value straddling the boundary.
  dbbat merges the fragments into one packet so it can patch `AUTH_SVR_RESPONSE`
  contiguously, then **re-fragments at the upstream's original boundaries** before
  forwarding — a single merged packet exceeds the client's negotiated SDU and is rejected
  with `ORA-12592` ("bad packet").

#### OCI break/reset before AUTH Phase 2 — root cause and fix

The OCI client sends an inline break/reset marker pair (`01 00 01` / `01 00 02`) before
AUTH Phase 2 **when it rejects the challenge dbbat sent** — overwhelmingly because the
challenge's trailing end-of-call summary width did not match the client's negotiated TTC
caps (fixed above). After the resync the client waits for the aborted call's completion,
which dbbat does not synthesize, so the session stalls or ends with `ORA-03106`. This was
**historically mis-attributed to the TCP-urgent (out-of-band) break probe** — but a spy
relay with `SO_OOBINLINE` + `SIOCATMARK` on both legs observed **zero** urgent bytes during
the failing handshake, and the corrected proxy now works end-to-end through an
OOB-stripping TCP relay (a faithful NodePort/NLB stand-in). No OOB bridging, no
`DISABLE_OOB`, and no OOB-preserving ingress are required. `readPhase2Packet` still answers
an inline break with a reset marker for resync robustness. Thin clients never send this
marker pair and are unaffected.

Cluster verification over the Kubernetes NodePort from the Windows host
(`C:\oracle\instantclient_23_0\sqlplus.exe`) is **pending manual verification** — it was out
of scope for the automated run that landed these fixes. Runbook: deploy the image built
from this branch, then from the Windows host run
`sqlplus orauser/<key>@//<node>:<nodeport>/FREEPDB1` and confirm a `SELECT … FROM dual`
returns rows with no `ORA-03106` / `ORA-12592` / hang, and re-check go-ora /
python-oracledb thin / SQLcl for no regression.

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
Without this, python-oracledb thin rejected the AUTH OK with `DPY-4035`, JDBC thin / SQLcl
with `ORA-17401`, and sqlplus / OCI with `ORA-01017`. go-ora ignores the field, which is why
the earlier static-capture path worked for it while silently breaking everyone else. The
static `capturedAuthOKResponse` remains only as a fallback when no upstream packet was
captured. For OCI the AUTH OK arrives split across two Data packets, so the value is patched
on the reassembled packet and re-fragmented before forwarding — see "OCI wide encoding".

### Client compatibility

All four supported client families authenticate + query + capture end-to-end against Oracle
23ai through the proxy — see the table under "Client compatibility on Oracle 23ai" above.
The last holdout, sqlplus / OCI instant client, was fixed by the wide-encoding path plus the
four OCI-only fixes documented there; it no longer depends on OOB / `DISABLE_OOB`. Against
Oracle 19c the historical behavior still applies.

### Per-user O5LOGON key

dbbat picks the connecting user's first API key with an O5LOGON verifier when generating the AUTH challenge — see the `O5LOGON verifier loaded` info log. That specific key (and only that one) is the password your Oracle client must supply: the salt sent in the challenge is bound to it, so any other API key fails to decrypt. Multi-key support is not yet implemented (it would require all of a user's keys to share one salt, since the challenge can only carry one).
