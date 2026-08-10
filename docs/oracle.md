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
| 0x03 | 0x4e | **Re-execute + fetch an already-parsed SELECT cursor** (no SQL) |
| 0x03 | 0x04 | **Re-execute an already-parsed non-SELECT cursor** (no SQL) |
| 0x03 | 0x76 | AUTH Phase 1 |
| 0x03 | 0x73 | AUTH Phase 2 |
| 0x03 | 0x09 | Session logoff (**not** a cursor close — see "Closing cursors") |
| 0x04 | — | **OER — error/status** (carries DML row count or ORA error) |
| 0x08 | — | Server response (carries an embedded OER on v315+) |
| 0x09 | — | Close/marker |
| 0x10 | — | **Query result with row data** |
| 0x11 | — | Fetch rows |
| 0x11 | 0x69 | **Close cursors** (a count + a list of ids), often with an execute stapled behind it |
| 0xde | — | JDBC initial negotiation |

### SQL Extraction

SQL text is inside piggyback execute messages (func=0x03, sub=0x5e). The SQL is length-prefixed, but its exact offset varies by client driver:

| Client | SQL offset in TTC payload |
|--------|--------------------------|
| Python oracledb thin | ~50 |
| JDBC thin (ojdbc) | ~54 |
| Go go-ora | varies |

The robust approach: scan offsets 40-70 for a `decodeVarLen` + readable SQL text, then validate with `looksLikeSQL()` (checks for SQL keyword prefix). As a fallback, scan the entire payload for SQL keywords (`SELECT`, `INSERT`, etc.) and extract until end of printable ASCII.

### Cursor re-execution (what clients actually send)

A client that has already parsed a statement re-runs it by naming its **cursor
id alone** — the statement text is never resent. dbbat has to recognise that or
the whole statement gate (read_only, block_ddl, approval holds) applies to the
parse only.

Measured, not assumed. Five recordings against Oracle Free 23ai, one per client,
each parsing `SELECT 1 AS n FROM dual` (or an `INSERT`) once and then running it
three more times — regenerate with:

```bash
docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
go test -tags capture -timeout 300s -run 'TestCapture_.*Reexec' -v ./internal/proxy/oracle/
```

| Client | Re-executes by cursor id? | Frame on the wire | Fixture |
|--------|---------------------------|-------------------|---------|
| go-ora v3 (prepared SELECT) | **yes**, every run after the first | func `0x03` sub `0x4e` | `testdata/go_ora_cursor_reexec.pcapng` |
| go-ora v3 (prepared INSERT) | **yes** | func `0x03` sub `0x04` | `testdata/go_ora_dml_cursor_reexec.pcapng` |
| python-oracledb thin (plain `cur.execute()` loop) | **yes** — no prepared-statement API needed, its per-connection statement cache does it | func `0x03` sub `0x4e` | `testdata/python_thin_cursor_reexec.pcapng` |
| JDBC thin / ojdbc11 (cached `PreparedStatement`) | **yes** | func `0x03` sub `0x4e` | `testdata/jdbc_thin_cursor_reexec.pcapng` |
| sqlplus / OCI thick | **no** — resends the full statement text on every run | func `0x11` sub `0x69` with SQL | `testdata/sqlplus_cursor_reexec.pcapng` |

So this is not an exotic shape: it is what every thin client does by default, and
python-oracledb reaches it without the caller asking for a prepared statement.

**None of them sends the SQL-less `OALL8` (func `0x0E`, SQL length 0)** the gate
was originally written against — that is the legacy pre-v315 framing of the same
idea. It is still handled (`decodeOALL8` → `OALL8NoSQLError`), kept as defence in
depth for older clients, but it was not observed from any client tested here.

Frame layout (`decodeCursorReexec`), identical for both sub-ops:

```
[0]    0x03            piggyback
[1]    0x4e | 0x04     sub-op
[2]    seq             TTC sequence number
[3]    0x00            present only from TTC version 18 (v315+) on
[4..]  cursorID, rowsToFetch, execOptions, execFlags   — TTC compressed ints
```

The four integers must consume the frame exactly; a trailing byte means it is
some other sub-op and it is left alone.

#### Learning the cursor id

Only the legacy `OALL8` carries the cursor id on the *request*. The piggyback
exec (`0x03`/`0x5e`) and the JDBC exec (`0x11`/`0x69`) send the SQL with no id —
the **server** allots one and reports it back — so without reading the response
dbbat has a statement with no id and, later, an id with no statement.

`learnCursorID` reads it off the first response to each execute
(`findCursorIDInResponse`): the OER's seventh field. The scan is anchored rather
than trusting — seven compressed ints, error code success or ORA-01403, a
sequence number inside its 16-bit field, a 16-bit cursor id, first match wins —
because a run of row bytes can otherwise parse as an OER. It runs at most once
per statement.

Every one of those bounds is load-bearing in **both** directions. Too loose and
row bytes are mistaken for the OER; too tight and the genuine OER is skipped and
the cursor is never learned. That second failure is not theoretical:

> **The sequence-number bound used to be 255**, on the belief that TTC numbers
> calls with a byte that wraps. It does not. That field is the end-to-end ECID
> sequence (`SummaryObject.EndToEndECIDSequence` in go-ora), a **uint16** that
> counts up across the whole session. A session crosses 255 after a few dozen
> statements, and from there every OER was rejected — learning silently switched
> off for the rest of the session.
>
> It did not show up as an unknown cursor, which is why it survived. Oracle
> **recycles cursor ids**, so the re-executions that followed found a *stale*
> tracker entry and resolved to whatever statement last held that id: the gate
> ran the wrong SQL and `/queries` recorded the wrong SQL. Caught mid-churn,
> five runs of `SELECT 1 AS n FROM dual` were all gated as
> `SELECT 35 AS churn FROM dual`.
>
> The sequence bound is fixed, and so is the masking mechanism: dbbat now
> decodes the client's whole close list (below), so a cursor the client closed
> leaves the tracker instead of lingering to answer for a recycled id, and an
> overwrite that changes the SQL behind an id is logged at WARN
> (`cursor id recycled onto a different statement`) instead of passing silently.

#### Closing cursors

A client tells the server it is done with cursors through the **close-cursors
piggyback** — message type `0x11` (`TNS_MSG_TYPE_PIGGYBACK`), function `0x69`
(`TNS_FUNC_CLOSE_CURSORS`). It is a *list*, and clients batch: `dbeaver.pcapng`
closes three cursors in one frame.

```
[0]    0x11            message type: piggyback
[1]    0x69            function: close cursors
[2]    seq             TTC sequence number
[3]    0x00            token byte — 23ai-era clients only
[..]   0x01            pointer flag
[..]   count           TTC compressed int
[..]   count x id      TTC compressed ints
[..]   (optional)      the next TTC message in the same packet
```

Unlike a re-execution the list is **not** required to consume the frame: a
client with a statement to run staples that execute behind the close list in the
same packet (`… 03 5e <execute>`), which is the same frame dbbat also reads as
the JDBC/DBeaver execute. Wire order is closes first, and dbbat follows it, so
the tracker drops an id before the execute behind it can be handed the same one.
What *is* required is that the list itself be complete and plausible — pointer
flag present, bounded count, every id inside 16 bits — because a half-read list
would evict cursors the client still holds, and an entry evicted early turns a
correctly-gated re-execution into ORA-01031.

> **Func `0x03` sub-op `0x09` is the session logoff, not a cursor close.** dbbat
> read it as one, taking the byte after it — the TTC sequence number — for a
> cursor id, so every session teardown evicted an unrelated tracker entry while
> the real close lists went unread. Every recording in `testdata/` carries
> exactly one of these frames, as the last thing the client sends, three or four
> bytes long: there is no room in it for a cursor id at all.

sqlplus (OCI thick) sends the same op in the **wide** encoding — an 8-byte
pointer sentinel and little-endian 32-bit count and ids. dbbat rejects that
rather than guessing at it; sqlplus never re-executes by cursor id (it resends
the statement text every run, see the client table above), so an entry it leaves
behind cannot mis-resolve anything.

There is deliberately **no cap** on `s.tracker.cursors`. A cap only buys memory,
and every entry it evicted would convert a correctly-gated re-execution into a
refusal. Instead `TestIntegration_CursorIDLearningMissRate` asserts the tracker's
peak size stays far below the 40 cursors its statement-cache-churn workload
opens and closes.

One gotcha the fixtures pinned: the OER **end-of-call bit is not universal**.
go-ora's connections carry `CallStatus 0x10005`, python-oracledb's carry
`CallStatus 1`. Query completion still keys off the bit (unchanged); the cursor-id
lookup deliberately does not, which is why `decodeOERFieldsAt` is split out of
`decodeOERAt`.

A re-execution naming a cursor dbbat cannot resolve goes through
`refuseUnknownCursor`, exactly like the SQL-less `OALL8` and a fresh-query
`OFETCH`: refused under a grant carrying a statement-shaped control, forwarded
with a WARN under one carrying none. See `docs/approvals.md`.

##### How reliable learning actually is: the numbers

Because failing that frame closed is only safe if learning is reliable, it was
measured rather than argued. `TestIntegration_CursorIDLearningMissRate` (build
tag `integration`) stands up a real Oracle plus the real proxy and drives a
client through a prepared-SELECT loop, bind-heavy re-executions, three cursors
interleaved on one session, DML, an anonymous PL/SQL block, a REF cursor, a
statement retried after it failed, and a statement cache churned past 40
statements. It counts the proxy's own log records.

Against `gvenzl/oracle-free:23-slim`, after the sequence-number fix:

| Client | Parses seen | Cursor ids learned | Re-executions | Naming an unknown cursor |
|--------|-------------|--------------------|---------------|--------------------------|
| `go-ora` v3 | 57 | 53 | 64 | **0** |
| `python-oracledb` thin 3.4.2 | 58 | 55 | 60 | **0** |

The parses that learned nothing are **exactly** the statements that failed
(`DROP TABLE` on a missing table, three retries of a `SELECT` on a missing
table): their OER carries a real ORA code, which the scan refuses to read an id
from, and no client re-executes a statement that errored. The test asserts that
list exactly — which is what catches a learning regression even when a recycled
cursor id hides it behind a stale entry.

Before the fix the same run learned 49 of 57, and the four extra misses did
**not** appear as unknown cursors: they appeared as re-executions gated against
the wrong statement.

Reproduce with:

```bash
ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim \
  go test -tags integration -timeout 40m -count=1 -v \
  -run TestIntegration_CursorIDLearningMissRate ./internal/proxy/oracle/
```

`CURSOR_TRACE=1` additionally dumps the ordered parse/learn/re-execute trace,
which is how the stale-entry mis-resolution above was spotted. The
`python-oracledb` half skips itself when the module is not installed (that is
the case in CI); the recorded python and JDBC captures replayed below are the
always-on half of the same evidence.

Replayed by `cursor_reexec_replay_test.go`, which drives the recordings through
the real client/upstream intercept paths and asserts the re-executions are
recognised, resolved to the right statement, and refused under `read_only`.

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
Verified against `testdata/go_ora_compressed.pcapng`
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
and verified end-to-end against `testdata/go_ora_numbers.pcapng`
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

Captured rows do not go to the database one at a time. `captureRow` hands each
row to the process-wide batching writer (`internal/proxy/shared/rowwriter.go`),
which flushes ~1000 rows (or ~8 MB) at a time and never blocks the capture
path: on a full queue the row is dropped and the query is flagged
`results_dropped`. The parent `queries` row is still created before the first
row is queued (`persistQueryRecord`), because `query_rows.query_id` is a
foreign key, and `completeQuery` waits on the writer's flush barrier before
marking the query complete. Before this, every captured row cost a synchronous
INSERT round-trip inline with the proxy — up to `max_result_rows` (100 000 by
default) of them on a single query.

### Database resolution and shared service names

The service name from the TNS Connect descriptor is first matched against dbbat
database **names** (exact match), then against `oracle_service_name`. Several
dbbat logical databases can share one upstream `oracle_service_name` (e.g. five
schemas of a mutualized instance behind `MUTU01`). Because the database is
resolved at Connect time — before AUTH Phase 1 reveals the username — an
ambiguous service name is handled by deferring the final choice:

- all candidates must share the same upstream `host:port` (otherwise the
  connection is refused immediately — there is no single address to relay to);
- after AUTH Phase 1, the candidates are filtered by the connecting user's
  active grants: exactly one match selects that database; zero rejects with
  "no active grant"; several rejects with a message asking the user to connect
  with the unambiguous dbbat database name instead.

The `GET /api/v1/databases/{uid}/connection` endpoint therefore advertises the
dbbat logical name in the EZ-Connect string whenever it is EZ-Connect-safe
(letters, digits, `_`, `.`, `-`), falling back to the raw upstream service name
for names containing spaces or parentheses.

## Known Limitations

- **Any API key works for Oracle login (per-user salts)**: The Oracle username from TTC AUTH Phase 1 maps to the dbbat user (lowercased) for grant checks and connection tracking, and any of that user's API keys created since the per-user-salt scheme can authenticate — see "Per-user O5LOGON salts" below. Two caveats: keys created before the scheme (legacy per-key salts) still fall back to first-key-only behavior until a new key is created, and clients that send an empty `AUTH_PASSWORD` (SQLcl / JDBC thin 23c+) cannot be disambiguated — dbbat assumes the most-recently-created user-salt key.
- **Row capture is best-effort**: The TTC binary format varies across Oracle client versions. Some clients/query types may produce partial or no row capture. SQL text extraction works reliably across all tested clients.
- **Column names**: Real column names come from the describe column-definition records (`parseColumnDescribes` in `describe.go`), so single-char aliases (`SELECT level AS n`) and unnamed expressions (`SELECT count(*)`) get their true names and positions. Only genuinely unnamed expression columns fall back to a synthetic `COLn` label. If the records don't parse on some server layout, decoding falls back to heuristic name-scanning plus describe-header count padding, so the column count (and row framing) stays correct.
- **DML row counts**: INSERT/UPDATE/DELETE affected-row counts are captured from the v315+ OER status block (TTC func `0x04`, embedded in the execute Response) and stored as `rows_affected`. Failed statements record the ORA error text. See `ttc_oer.go`.
- **Bind values (parameterized queries)**: Bind values are captured from both the legacy `OALL8` execute path (`decodeBindValues`) and the v315+ **piggyback exec** path that modern clients use (`extractPiggybackBinds`, func `0x03` sub `0x5e`). The piggyback binds sit length-prefixed at the tail of the message; they're located as the suffix that parses as exactly as many values as there are distinct bind placeholders in the SQL, and each is decoded by content via `decodeOracleRawValue` (so a NUMBER bind like `42` renders as `42`, not hex). Verified against `testdata/go_ora_binds.pcapng` (`TestDumpReplay_Binds`). Captured binds are now persisted to `queries.parameters` (`formatOracleBinds` wired into `persistQueryRecord` and `completeQuery`), so the API (`GET /api/v1/queries/:uid`) and the UI Parameters card report them. Not yet handled: binds over ~253 bytes (extended length encoding) and full type-aware decoding from the bind-definition records.
- **Temporal types**: DATE, TIMESTAMP, and TIMESTAMP WITH TIME ZONE decode in captured results, verified end-to-end against `testdata/go_ora_temporal.pcapng` (`TestDumpReplay_Temporal`). The tz form renders the local wall clock plus its numeric offset, honouring byte 11's `0x40` "time in zone" flag (prefix stored as local vs UTC). Named-region time zones fall back to the stored wall clock without an offset suffix.
- **Large result sets**: The QueryResult (func `0x10`) row area and continuation packets (func `0x06`) share one decoder (`parseRowStream`) that walks the full compressed row stream — length-prefixed values plus the `0x15 [flag] [count] [bitmask] 0x07` column-compression descriptors between rows. A 400-row single-packet result is captured end-to-end against a live-Oracle ground-truth fixture (`testdata/go_ora_largeresult.pcapng`, `TestDumpReplay_LargeResultRows`). Multi-TNS-packet (small-SDU/JDBC) result sets reuse the same decoder via the continuation path; their per-row correctness is not yet ground-truth-verified.

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

### Integration tests

`internal/proxy/oracle/*_integration_test.go` and `integration_test.go` are behind the
`integration` build tag and start a real Oracle in Docker via testcontainers, so `make test`
never runs them. CI does run `go vet -tags integration ./...` so the tagged build cannot rot
silently — but that only compiles them.

```bash
# default image: gvenzl/oracle-free:23-slim (Oracle 23ai Free; amd64 + arm64)
make test-e2e-oracle

# pin the older 18c XE image — amd64 only, does not boot on Apple Silicon
ORACLE_TEST_IMAGE=gvenzl/oracle-xe:18.4.0-slim go test -tags integration -timeout 40m ./internal/proxy/oracle/
```

The default is the 23ai Free image on **every host and in every environment**:
it has an arm64 build, and 23ai is the version the proxy work is validated
against (see the client-compatibility table below). The previous default,
`gvenzl/oracle-xe:18.4.0-slim`, is published for `linux/amd64` only and dies
during instance startup under emulation on Apple Silicon (ORA-27300 /
ORA-00442, surfacing as ORA-00442 / exit 186), which made the whole suite
useless on an M-series Mac.

18c XE coverage is **not** dropped, it is pinned: `.github/workflows/integration.yml`
runs the Oracle suite twice on `ubuntu-24.04` — once on the suite default, and
once with `ORACLE_TEST_IMAGE=gvenzl/oracle-xe:18.4.0-slim`.

| Variable | Purpose |
|----------|---------|
| `ORACLE_TEST_IMAGE` | Container image to start (default `gvenzl/oracle-free:23-slim`) |
| `ORACLE_TEST_SERVICE` | PDB service name; inferred from the image otherwise (`XEPDB1` for XE, `FREEPDB1` for Free, `ORCLPDB1` for enterprise) |

#### A refusal does not end the client's call

`TestIntegration_BlockedStatementsAreLogged` drives a real go-ora client through
the proxy under a `read_only` and then a `block_ddl` grant. Both refusals are
enforced (nothing reaches upstream) and both are logged (a `queries` row
carrying the refusal as `error`, an ordinary link in the connection's HMAC
chain) — but **the client never gets its error back**. `session.writeTTCError`
emits a TTC Response (0x08) where a server ends a call with an OER (0x04), so
the statement hangs until the client's own timeout, if it has one. The test
abandons each refused statement after 20s rather than asserting a client-side
error, and says so where it does it. See
`specs/todos/2026-08-10-17-oracle-refusal-frame-hangs-the-client.md`.

Note that `buildTNSConnect` in the test file announces TNS version 313 on purpose: from 315
onwards Oracle expects the extended Connect format (see "TNS Version >= 315" above), which
that simplified builder does not emit, and a 23ai listener drops the connection outright.

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

Query/response interception in proxy mode is **best-effort observability**: whatever it
decodes, it forwards byte-exact. A decode error must never break the connection — and a
*panic* must never take down the whole process. Two guards enforce this:

- `dlc()` (`describe.go`) rejects a negative length. SQLcl/ojdbc negotiates a high
  TTCVersion, so the server's column-describe records carry the modern domain/annotation
  layout the parser misaligns on; a NUMBER scale's `-127` sentinel was then read as a
  length, producing `data[:-127]` — a panic that crashed the **entire** dbbat process for
  *all* connections. Now it returns nil and `parseColumnDescribes` bails out.
- `interceptClientMessage` / `interceptUpstreamMessage` each `recover()` from any panic in
  the decode path and forward the packet unchanged.

See `sqlcl_regression_test.go` for both guards (real SQLcl 26.1.2 fixtures).

"Best-effort" describes what happens when the decode **fails**, not what happens when it
succeeds. A statement dbbat *did* decode is enforced: `read_only`, `block_copy`,
`block_ddl`, the Oracle blocklist and the grant's approval patterns all run before the
packet is forwarded, and a refusal is answered with a TTC error instead
(`sendOracleError`). The three statement-carrying ops — `OALL8`, the v315+ piggyback exec
(`func=0x03` / sub-op `0x5e`) and the JDBC exec (`func=0x11` / sub-op `0x69`) — go through
the identical normalize → validate → hold → record sequence in `intercept.go`; adding a
fourth means wiring the same sequence, not just a `persistQueryRecord()` call. The
corollary is that a frame whose SQL cannot be extracted is neither logged nor enforced —
see the Oracle caveat in `docs/approvals.md`.

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

#### A mid-fetch `0x08` is row data, not a Response

The byte at TNS payload offset 2 is only a TTC function code **at a call boundary**. While
a result set is streaming, that byte is row-stream content — an 8-byte value's length
prefix, or the `08 01 06` end-of-rows footer — so a packet starting with `0x08` used to be
routed to `handleResponse` and read through the *legacy* fixed-offset Response layout:
`payload[2:6]` as an error code, `payload[12:14]` as an error flag, `payload[14:16]` as a
message length. On real row data that reliably produces a nonzero code, a nonzero flag and a
several-hundred-byte "message" of verbatim column-compressed row bytes. Observed in
production on a healthy 57.9 s `SELECT`: `rows_affected = 5316` plus a 772-byte binary
"error". Because the fabricated error completed the query, `pendingQuery` was cleared and,
for the rest of the fetch, row capture stopped (without setting `results_truncated`),
`s.guard.Check()` — gated on `pendingQuery != nil` in `upstreamToClient` — stopped enforcing
the grant's byte cap mid-stream, and the remaining bytes were charged to the next query.

Two independent fixes, either of which alone would have prevented the production symptom:

- `handleResponse` checks `rowStreamActive()` (a pending query whose cursor already has
  column definitions). Mid-stream, only an **embedded OER** — whose end-of-call bit
  `decodeOERAt` verifies — is honoured as a call boundary; anything else is decoded as
  continuation row data.
- `decodeTTCResponse` (and `parseResponseError`) must *prove* a legacy error before
  reporting one: a plausible code (< 100000) **and** an extracted message that is a
  printable `ORA-`/`PLS-`/`TNS-` diagnostic. Otherwise the payload is rejected with
  `ErrNotLegacyResponse` rather than surfacing misread bytes. No message is synthesized
  from the code alone. Every `0x08` Response in every capture fixture used to decode as a
  bogus `ORA-<huge number>`; a replay test now asserts none of them do.

As a last line of defence, `shared.SanitizeQueryError` guards every protocol's completion
path. It is deliberately **not** a plain "valid UTF-8 or drop" check, because dbbat does not
know the session charset: a genuine diagnostic from a WE8ISO8859P1 session (the common case
on European estates) is not valid UTF-8, and dropping it would silently lose real errors.
Instead:

- a string carrying **control bytes** (C0, DEL, C1) is dropped — no diagnostic contains
  them, and misread row data always does;
- a string with a **few undecodable bytes** is kept, with those bytes replaced by U+FFFD:
  `ORA-00001: contrainte unique viol<?>e` is more useful to an operator than nothing;
- a string that is **more than a quarter** undecodable is binary, not a sentence with
  accents in it, and is dropped.

Both outcomes are logged at debug with the length only, never the bytes.

A related, pre-existing limitation sits upstream of that gate: `extractORAMessage` truncates
an OER's message at the first non-printable byte, so a Latin-1 accent inside a genuine ORA
message ends the extracted text there. The sanitizer never sees those bytes.

Also note `cleanup()` now flushes a still-pending query. A client that disconnects mid-fetch
used to leave the query row forever incomplete — `duration_ms` NULL and its bytes never
charged to the connection or the grant. The fabricated completion removed above used to
(wrongly) close such rows, which is why the leak had gone unnoticed.

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

### `ORA-03120` from a 5-character username (thin clients)

`ORA-03120: two-task conversion routine: integer overflow`, preceded by two TNS
break markers, is the generic "you sent me a message I cannot parse" answer. It is
documented above as a *capability* mismatch, but there is a second, much dumber way
to earn it, fixed 2026-08-10 — worth knowing before you go caps-hunting.

The upstream AUTH Phase 1 dbbat sends is the client's own packet with the username
swapped for the database user (`rewriteAuthPhase1Username`). A thin client's Phase 1
preamble looks like this (go-ora v3, captured live against 23ai Free):

```
03 76 01 00 01 | 01 <user_id_len> | 01 <logon mode> | 01 01 <numPairs> 01 01 | <clr len> username
```

`numPairs` is 5 for go-ora, and it sits **closer to the username than
`user_id_len` does**. `findUserIDLenPos` used to scan backward from the username
for the first byte equal to the old length, so a 5-character login name matched the
pair count instead: the rewrite bumped the number of KV pairs and left the length
stale, the upstream read a 5-byte name out of a 6-byte field, and out came
`ORA-03120`. The wide (OCI) branch of that function already anchors rather than
scans for exactly this reason — its comment even names the 5-char `admin` case —
but the thin branch did not, so `admin`, dbbat's own default user, could not log in
through the proxy with any thin client.

The tell that distinguishes this from a caps mismatch: the pre-auth relay exchange
is byte-for-byte identical to a working direct client session (compare with a
recording TCP relay in front of the same server), and the failure survives forcing
the synthetic `buildClientAuthPhase1` fallback. If both hold, look at field values
in the outgoing Phase 1, not at capabilities.

### Connectivity check: fast login hides `ORA-01017`

The connectivity check (`internal/proxy/conncheck`) is the one place dbbat logs in to Oracle
as an ordinary client, through `upstream.ConnectOracle` / go-ora, rather than by relaying a
downstream session. Its connect string sets **`FAST LOGIN=FALSE`**, and that is load-bearing.

Oracle 23ai offers a one-round-trip logon ("fast login") that folds the protocol negotiation
into the auth exchange. go-ora enables it by default, and its fast path then reads the
server's reply as a negotiation message *without first checking whether it is a TTC error
(message code 4)*. The server answers a wrong password with a perfectly readable
`ORA-01017`; go-ora rendered it as:

```
message code error: received code 4 and expected code is 1
```

which the classifier can only call `db_handshake_failed` — pointing the admin at the network
when the problem is the credentials. With fast login off the real `ORA-01017` comes back and
the check answers `db_auth_failed`, while an unknown service still yields `ORA-12514` and
stays `db_handshake_failed`. The cost is one extra round trip, on a check that runs when
someone presses "test connection". Pre-23ai servers never offered fast login, so this is a
no-op against them.

### Client compatibility

All four supported client families authenticate + query + capture end-to-end against Oracle
23ai through the proxy — see the table under "Client compatibility on Oracle 23ai" above.
The last holdout, sqlplus / OCI instant client, was fixed by the wide-encoding path plus the
four OCI-only fixes documented there; it no longer depends on OOB / `DISABLE_OOB`. Against
Oracle 19c the historical behavior still applies.

### Per-user O5LOGON salts (any API key works)

The O5LOGON challenge can only carry **one** salt and **one** encrypted server
session key, and both must be committed before the client reveals which API key
it holds. Historically each key had its own random salts, so dbbat picked the
user's first verifier-bearing key and only that key could log in.

Since the per-user-salt scheme, the salts live on the **user**
(`users.protocol_data.oracle.o5logon_user_salt_6949` / `_18453`, generated
lazily at API key creation) and every new key derives its 6949 + 18453
verifiers from them (`OracleAPIKeyData.user_salt = true`; the verifiers stay
per-key, AES-GCM encrypted with AAD bound to the key prefix). The challenge is
built from the most-recently-created user-salt key, and **all** user-salt keys
remain candidates:

- **Phase 2 with `AUTH_PASSWORD`** (go-ora, python-oracledb thin, sqlplus):
  each candidate's view of the combined key is rebuilt by decrypting the
  challenge ciphertext under that candidate's verifier (`CloneForCandidate`),
  `AUTH_PASSWORD` is trial-decrypted, and only a plaintext that passes
  `VerifyAPIKey` authenticates. Works for both 6949 and 18453/customHash.
- **Phase 2 with empty `AUTH_PASSWORD`** (SQLcl / JDBC thin 23c+): the client
  never sends the password text, so candidates cannot be disambiguated. dbbat
  deterministically assumes the **most-recently-created** active user-salt key
  (logged as `empty AUTH_PASSWORD — cannot disambiguate candidates`); proof of
  knowledge stays implicit via the `AUTH_SVR_RESPONSE` marker check.
- **Legacy keys** (created before the scheme, `user_salt` absent): unchanged
  fallback — the first verifier-bearing key is the single candidate, no forced
  rotation. Creating any new key upgrades the user to multi-key login.
