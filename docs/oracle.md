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
(`findCursorIDInResponse`, a thin wrapper over `findPlausibleOERInResponse`):
the OER's seventh field. The scan is anchored rather than trusting — seven
compressed ints, error code success or ORA-01403, a sequence number inside its
16-bit field, a 16-bit cursor id, first match wins — because a run of row bytes
can otherwise parse as an OER. It runs at most once per statement.

Query completion out of a row stream shares that same scan; see "the OER
end-of-call bit is not universal" below.

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

sqlplus (OCI thick, and by extension SQL*Developer via the OCI driver and
Instant Client generally) sends the same op in the **wide** encoding — the same
shape the AUTH path already knows (`payloadUsesWideKVEncoding`,
`replaceAuthKVValueWide`):

```
[0]    0x11            message type: piggyback
[1]    0x69            function: close cursors
[2]    seq             TTC sequence number
[3]    0x01            constant
[4]    seq+1           the NEXT TTC message's sequence number
[5..12] fe ff ff ff ff ff ff ff   8-byte pointer sentinel
[13..16] count          uint32 little-endian
[17..]   count x id     uint32 little-endian each
```

The two bytes at `[3..4]` were not guessed at: pinned against every
close-cursors frame in `testdata/sqlplus_cursor_reexec.pcapng` (client frames
9, 12, 14, 16) plus the piggyback execute-with-SQL header (func `0x03` sub
`0x5e`) stapled behind three of them, whose own header carries the identical
two-byte pad ahead of its own sentinel — the first byte is always a constant
`0x01` and the second always equals *that header's own* sequence number plus
one, i.e. the sequence number the next TTC message on the wire will carry.
That makes it the wide framing's own header padding, not something specific
to the close-cursors op, and `decodeCloseCursors` validates the shape (not
just skips two bytes) before reading the count and ids as little-endian
uint32s. The guards mirror the thin path exactly — bounded count, every id
inside 16 bits, enough bytes for the whole list — and a payload that doesn't
fit either shape still returns `ErrNotCloseCursors` and deletes nothing.

This is defense in depth rather than something load-bearing: sqlplus never
re-executes by cursor id (it resends the statement text every run, see the
client table above), so a tracker entry it leaves unread cannot mis-resolve a
re-execution. What it bounds is a tracker that would otherwise grow for the
life of a long OCI session, and a mixed deployment where some *other*
OCI-based client re-executes by id.

There is deliberately **no cap** on `s.tracker.cursors`. A cap only buys memory,
and every entry it evicted would convert a correctly-gated re-execution into a
refusal. Instead `TestIntegration_CursorIDLearningMissRate` asserts the tracker's
peak size stays far below the 40 cursors its statement-cache-churn workload
opens and closes.

One gotcha the fixtures pinned: the OER **end-of-call bit is not universal**.
On the successful calls the first fixtures covered, go-ora's connections carry
`CallStatus 0x10005` and python-oracledb's carry `CallStatus 1–2`. That is why
`decodeOERFieldsAt` is split out of `decodeOERAt` — the strict decoder, which
demands the bit, cannot see those OERs at all.

**It is not a client trait either, and reading it as one cost a second set of
data.** `testdata/python_thin_failed_stmt.pcapng` and
`testdata/go_ora_failed_stmt.pcapng` record the same six failures on each
client (regenerate with `go test -tags capture -run 'TestCapture_.*FailedStatements'`),
and the bit follows the *call*, identically on both:

| Failing statement | ORA | `CallStatus` (both clients) | Bit |
|---|---|---|---|
| `DROP TABLE` of a missing table | 00942 | `0x10001` / `0x10005` | yes |
| `SELECT` against a missing table | 00942 | `5` | no |
| `INSERT` violating a unique key | 00001 | `1` / `5` | no |
| `SELECT 1/0` — fails producing the row | 01476 | `1` | no |
| PL/SQL `RAISE_APPLICATION_ERROR` | 20001 | `1` | no |
| PL/SQL block that will not compile | 06550 | `1` | no |

Every one of them arrives as a **standalone func `0x04`** after a marker
exchange — never embedded in a Response — so `handleOERStatus` is where a
failure's text is won or lost, and until this was measured it was lost on
*every* client, go-ora included: five of the six shapes were dropped by
`decodeOERAt`, the statement stayed pending, and the next statement's
`flushPendingQuery` closed it as a **success with no error at all**.

`decodeErrorOER` is what reads them now. It does not loosen an anchor; it
replaces the bit with the proof `decodeTTCResponse` is held to — a real error
code inside Oracle's range, a printable `ORA-`/`PLS-`/`TNS-` diagnostic in the
tail, and that diagnostic **naming the very code the fields reported**
(`ORA-00942` for 942, `ORA-06550` for 6550). A run of row bytes that decoded as
seven ints would also have to be followed by the ASCII spelling of the number
its fourth field landed on. It is errors-only: a bit-less standalone OER
reporting success or ORA-01403 still completes nothing, because nothing proves
those bytes.

Cursor-id learning never required the bit. Query completion did, and that cost
real data: for a full release every INSERT/UPDATE/DELETE run by a
python-oracledb thin client fell through `handleResponse`, stayed pending, and
was closed only by the *next* statement's `flushPendingQuery` —
`rows_affected` NULL and a `duration_ms` measuring the client's think time (one
live UPDATE was logged at 74 s because the session then sat idle). A session
whose last statement was such a DML had it sealed by `cleanup()` instead, timed
to the disconnect. dbbat was reading cursor 4 off the very OER whose
`CurRowNumber` it refused to believe.

Both callers now share one anchored scan, `findPlausibleOERInResponse`, and what
separates them is **where** they may run it, not how far they trust the same
bytes:

| Path | Bit required? | Why |
|------|---------------|-----|
| `handleOERStatus` (standalone func `0x04`), **reporting success or ORA-01403** | **yes** | routed on byte 0 alone, so any row-value length prefix of `0x04` arrives claiming to be an OER, and a status carries no text to prove itself with |
| `handleOERStatus`, **reporting a failure**, mid-row-stream | **yes** | still row bytes; the strict decoder stays the only thing that may end a call there |
| `handleOERStatus`, **reporting a failure**, outside a row stream | no | `decodeErrorOER` proves the tail is a diagnostic naming the code the fields reported — see the table above |
| `handleResponse`, mid-row-stream | **yes** | the payload *is* row bytes; a `0x04` run inside it is data |
| `handleResponse`, outside a row stream | no | the payload is a return-parameter block, and the anchors above are what stands in for the bit |

Say the second row's consequence out loud rather than leaving it as a rule: a
failure raised **after rows have started flowing** is therefore recorded with
**no error at all** — the statement stays pending and the next one's
`flushPendingQuery` closes it as a success, exactly the behaviour this change
removed everywhere else. That is a deliberate trade, not an oversight: mid-fetch
a `0x04` run *is* row data, and reading one as a diagnostic is the production
incident described further down. No such failure has been measured (every shape
in the table above is raised before the first row, the divide-by-zero included,
because the server sends the OER *instead of* the QueryResult); measuring one is
`specs/todos/2026-08-13-09-oracle-mid-fetch-failure-records-no-error.md`.

Cursor-id learning is on none of those rows and was not touched: it reads
`findPlausibleOERInResponse`, which still refuses any OER reporting a real
failure, because such an OER assigns no cursor. That same property is why it
cannot be used to *ask* whether a Response carries a failure — it would always
answer no — and why
`TestDumpReplay_NoFailureArrivesEmbeddedInAResponse` scans with `decodeErrorOER`
instead, under a negative control that plants a real failure OER inside a real
Response and requires the scan to find it.

The live success OER is replayed in `oer_no_end_of_call_test.go`; its envelope
is derived from the captures in `testdata/` rather than copied from the
production dump, which is not in the repository. The failures are replayed
straight out of the two capture files in `failed_stmt_replay_test.go`, and
driven through the whole proxy against a live Oracle in
`failed_stmt_integration_test.go`.

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
- `callStatus` has the end-of-call bit `0x010000` set on some calls and not
  others. It is *not* the client discriminator an earlier reading of the
  fixtures took it for: on a successful call go-ora sees it and python-oracledb
  thin does not, but on a **failing** one both clients agree, and only a failed
  DDL carries it. `decodeOERAt` therefore uses it to reject stray `0x04` runs
  only where a false positive would be made of row bytes, and
  `decodeErrorOER` stands in for it on a proven diagnostic — see the tables
  under "the OER end-of-call bit is not universal". `ttc_oer.go`,
  `findOERInResponse` and `findPlausibleOERInResponse`.

#### Ending a call: the OER encoder

Every refusal dbbat issues — `read_only`, `block_ddl`, `block_copy`, a quota, an
expired or revoked grant — has to *end the client's call*, and the only frame
that does that is the same OER. `ttc_oer_encode.go` is the writing half of the
decoder above: `writeTTCError` builds a message-type-`0x04` summary object and
sends it in a **v315+ framed** Data packet (the legacy 2-byte length form is
read by a modern client as a multi-megabyte packet — the same trap
`sendAuthFailed` documents).

Most of the object is fields dbbat has nothing to say about, and it writes zero
into all of them. That is what makes the encoder tractable: a TTC compressed
zero and a raw zero byte are the same byte, so every field whose *width* is
conditioned on the negotiated capabilities costs nothing to get "wrong" as long
as its value is zero. Only the call status, the error code, the message text and
the **call number** have to land exactly.

The call number is the exception to "zero is free", and it was found by
measuring, not by reading a parser. It is the TTC sequence number of the request
being answered — the byte the client put at offset 2 of its own op header
(`03 5e 07 00` → 7) — and every real server OER carries it back: 8 and 4 in the
two fixtures in `ttc_oer_encode_test.go`, 45 in the OCI encoding at a constant
offset. **ojdbc 26.1 refuses to read an error out of an OER whose call number is
not the one it sent** (driver versions throughout this section are attribution,
not something the tree pins — see "Which ojdbc these results are attributed to"
below): `T4CTTIfun.receive` calls `processError()` only when
`oer.callNumber == this.sequenceNumber` and otherwise goes to
`handleOutOfSequenceError`, which surfaces `ORA-18745: Execution error in
sessionless transaction piggybacked call` with the real ORA-01031 demoted to its
*cause*. With dbbat's zero that is exactly what a live JDBC client reported for
every refusal — the call did end and the session stayed usable, but the headline
error named the wrong thing. `clientCallNumber` picks the number off the client's
own request; a packet may staple several ops, and the one the client is waiting
on is the **last** (JDBC sends its execute behind a close-cursors list:
`11 69 06 00 …closes… 03 5e 07 00 …`, and 23ai answers that packet with 7).
go-ora v3, python-oracledb thin, sqlplus and ojdbc 23.2 never check it.

Three variations do change the frame, and they are resolved differently:

| Variation | Where it comes from |
|---|---|
| call status present, ECID sequence present | `ServerCompileTimeCaps[15]&1` / `[16]&1`, off the relayed Set Protocol reply |
| TTC field version (only the `>= 7` boundary matters) | `min(client, server) CompileTimeCaps[7]`, the client's half off its Set Data Types request |
| extra tail fields, fixed-width encoding, end-of-response marker | **learned from the upstream's own OERs** |

The third row is learned rather than derived because no capability rule predicts
it. Against one Oracle 23ai Free instance, go-ora v3 and python-oracledb thin
both negotiate `CompileTimeCaps[7] = 24`, and the server sends python-oracledb
two extra fields between the trailing RetCode/row-count pair and the message
text (the "fields added in Oracle Database 20c" its parser reads) and go-ora
none. Patching each differing capability byte of one client's array to the
other's did not move the boundary. So dbbat copies what the upstream actually
emits: the upstream leg negotiated with *this client's* forwarded capabilities,
so its OERs are shaped exactly the way the client parses one. The AUTH response
carries the first sample, before any statement runs
(`readUpstreamAuthMessages`), and every response refreshes it
(`session.learnOERTail`).

OCI/sqlplus is a third encoding again: the same fields in the same order,
marshaled as **fixed-width little-endian** integers (only the row count stays
compressed, at a constant offset 70), and its messages end with the TTC
end-of-response marker `0x1d` even after dbbat has cleared
`HAS_END_OF_RESPONSE` from the Accept. Both are learned in the same pass;
with a byte-perfect compressed OER, or a byte-perfect fixed-width one with no
trailing marker, sqlplus hangs exactly as it did on the old frame.

Verified end to end against Oracle 23ai Free with **four** live clients — go-ora
v3, python-oracledb thin, sqlplus (OCI) and JDBC thin (ojdbc11 23.2.0.0 and
26.1 when it was taken; re-run since on 23.7.0.25.01 — see the version note
below): all four surface ORA-01031 and keep the session usable.

JDBC needed no fourth `oerShape` variation. It negotiates the same compressed
encoding python-oracledb does, two extra tail fields included
(`learned OER tail shape from upstream` reports `extra_tail_fields=2` on a JDBC
session), which is what
`TestIntegration_BlockedStatementRefusesJDBCThin` asserts rather than assumes.
What it did need was the call number above: measured on 26.1 the refusal came
back as ORA-18745 wrapping the real ORA-01031, and on 23.2 as a clean ORA-01031,
from the same dbbat frame (both attributions, again — see the version note
below). The JDBC case is skipped unless an Oracle JDBC driver is reachable, which
is the case in CI:

| Variable | Meaning |
|---|---|
| `ORACLE_TEST_OJDBC_JAR` | Path to an `ojdbc11.jar` (or any `ojdbc*.jar`). A `CLASSPATH` entry whose file name contains `ojdbc` is used when the variable is unset; a variable that points at a missing file fails the test rather than skipping it |

```bash
ORACLE_TEST_OJDBC_JAR=/path/to/ojdbc11.jar \
  go test -tags integration -timeout 40m \
    -run 'JDBCThin|RealOracleSessionTermination' ./internal/proxy/oracle/
```

That pattern picks up the refusal case here plus the two asynchronous ones and
the real-Oracle reference capture, all of which need the same jar — see the next
section.

###### Which ojdbc these results are attributed to

**No JDBC result in this document is pinned to a driver version by anything in
the tree.** Every JDBC test runs whatever jar `ORACLE_TEST_OJDBC_JAR` supplies
and none of them asserts on its version — pinning would mean checking
`DatabaseMetaData.getDriverVersion()`, which nothing does. So each result is
*attributed* to the driver that was on the machine when it was taken:

| Result | Taken on |
|---|---|
| the ORA-18745 wrap of a refusal stamped with call number zero, and the clean ORA-01031 from the same frame once the number was stamped; ojdbc 23.2 not checking the number at all | ojdbc11 26.1 and 23.2.0.0, as recorded when the call number was found |
| the two asynchronous refusals and the real-Oracle kill capture (2026-08-12) | **ojdbc 23.7.0.25.01** — the jar SQLcl 26.1 bundles, and the only one reachable on that machine |

The later measurement neither repeats nor retracts the earlier one. 23.7 carries
the same check the 26.1 finding rests on — `T4CTTIfun.receive` compares
`T4CTTIoer11.callNumber` against its own `sequenceNumber` and routes a mismatch
to `handleOutOfSequenceError`, which logs *"TTIOER call number {0} does not match
TTIFUN sequence number {1}"* — so "ORA-18745 did not appear" is meaningful under
it rather than vacuous. What no run on 23.7 did is *reproduce* the ORA-18745 wrap
itself; only its absence was observed.

##### An asynchronous refusal: which call number, and whether to send one at all

The call number above is the sequence number of the call *being answered*, which
raises the obvious question for the refusals that are not answers — a grant
revoked or expired while the client sits idle, and a byte quota tripped while
dbbat is relaying a reply. In both, `session.oerCallNumber` still holds the last
op the client sent, and it is not self-evident that ending *that* call is right.

The answer is that the question only ever has one live case, because **dbbat
never writes an OER outside a call**. Enumerated, the sites are:

| Site | Is the client inside a call? | Call number written |
|---|---|---|
| `gateStatement` / the `OFETCH` refusal, on the client leg | yes — the packet being refused is the call | the op just observed |
| `answerHeldRefusal`, on the client leg (a limit crossed mid-reply) | yes — the packet being refused is the call | the op just observed |
| `enforceMidStreamLimits`, on the response leg | the client is *inside* a call but not at a boundary in it | **none — the violation is held for the client's next call** |
| `onLimitViolation` (the limit watchdog: revocation, expiry, idle) | no | **none — it force-closes both sockets and writes no frame** |

The mid-reply case is the one that moved, and it moved because "inside a call" is
not the same question as "at a place a frame can be read". dbbat cuts into the
reply *before* the call's own end-of-call OER has reached the client, so the
client is indeed still parked in the receive for the op `oerCallNumber` names —
the number was right. But it is parked *mid-message*: a fetch reply is a TTC
message stream whose messages straddle TNS packets, and an OER written at a
packet boundary lands inside a half-delivered row batch. Measured, no client
could read it (below). So the violation is now **held** and answered on the
client's next call, where the boundary is not inferred but announced: a client
that sends a new op has by construction finished consuming the previous reply.
`observeClientCallNumber` runs on every client message, fetches included, so that
call's own number is what the frame carries.

The genuinely asynchronous case — the watchdog, with the client idle between
calls — resolves the other way: there is no call to end, and dbbat must not
invent one. An unsolicited OER on an idle socket is not read when it is sent; it
sits in the receive buffer until the client's *next* request and is then consumed
as that request's answer, carrying by construction the number of the *previous*
call. That is precisely the mismatch ojdbc's `handleOutOfSequenceError` turns
into ORA-18745, so a "graceful" error frame would be strictly worse than the
socket close, which surfaces as a plain ORA-17002/ORA-03113 I/O error. **Do not
add an error frame to `onLimitViolation`**; the code comment there says the same
thing.

###### What a real Oracle does, measured

`TestIntegration_RealOracleSessionTermination` puts ojdbc in front of Oracle 23ai
Free with **no proxy in between**, through a recording TCP tap
(`tap_test.go`), parks it between calls, and kills its session from a second
connection. All three forms agree on the part that matters: **not one byte is
pushed at the idle client.**

| Form | While the client is idle | At the client's next call |
|---|---|---|
| `ALTER SYSTEM KILL SESSION` | 0 bytes, socket held open | two Control packets (the break — ojdbc logs *"Break received from server. Responding with reset…"*) then a Data packet carrying `ORA-00028`, stamped with **that call's own** sequence number. Client reports a clean ORA-00028 |
| `ALTER SYSTEM KILL SESSION … IMMEDIATE` | 0 bytes, **socket dropped** | ORA-03113 |
| `ALTER SYSTEM DISCONNECT SESSION … IMMEDIATE` | 0 bytes, **socket dropped** | ORA-03113 |

The IMMEDIATE forms are what `onLimitViolation` imitates, and the measurement of
dbbat's own idle path matches them exactly: the tap records **zero** bytes
between the revoke and the client's next statement, the socket is dropped, and
ojdbc reports ORA-03113 — no ORA-00028, no ORA-18745.

###### The mid-reply refusal: right number, wrong place — and the fix

The mid-reply case first measured **worse than the reasoning predicted**, though
not in the way the reasoning worried about.
`TestIntegration_AsyncRefusalAgainstJDBCThin` trips a `max_bytes_transferred`
quota inside a streaming result set, with the same tap in front of the proxy.
With the refusal written where the violation was noticed:

- dbbat did everything the decision intended. The **inline** check fired (the
  watchdog did not), and wrote exactly one well-formed
  `ORA-00028: session terminated: bandwidth quota exceeded for this grant`,
  carrying a non-zero call number — read back off the wire, not off dbbat's logs;
- **no client reported it.** ojdbc answered `ORA-03113: database connection closed
  by peer` (`last_rpc=Fetch a row`, cause `ORA-17800: Got minus one from a read
  call`); go-ora, driven through the same trip, answered `driver: bad connection`.

The call number was *not* what broke it — that was the identified risk and it is
ruled out: a rejected call number produces ORA-18745, and no ORA-18745 appears
anywhere. What was wrong is **where** the frame went: dbbat cut in at a *TNS
packet* boundary, while a fetch reply is a *TTC message* stream whose messages
straddle packets (which is why `handleContinuation` carries `lastRow` state
across them). The OER therefore landed inside a half-delivered row batch, was
consumed as row bytes, and the client waited for a remainder that never came.
Two drivers reading it the same way is what made this the injection point and not
either driver's error handling.

**The fix is to wait for a boundary the client vouches for.** There is no
predicate to be had on the response leg: `parseContinuationRows` is a
best-effort scanner, `handleContinuation` recognises the end of a batch only by
finding the *text* `ORA-01403`, and `handleResponse` needs `rowStreamActive()`
precisely because a leading byte is not a reliable message type mid-stream. A
boundary inferred from those would be wrong exactly where it matters. But the
client announces one for free: **a client that sends its next TTC op has, by
construction, finished consuming the previous reply.** So
`enforceMidStreamLimits` arms a `heldRefusal` and keeps relaying, and
`interceptClientMessage` answers the next call with the ORA-00028 through the
ordinary `writeTTCError` path — the one already measured working on all four
clients — stamped with *that* call's number. It is also exactly what a real
Oracle does, one section above: `ALTER SYSTEM KILL SESSION` pushes nothing at the
parked client and answers its next call.

The cost is the tail of the fetch batch already in flight, bounded by the
client's fetch size. Three fail-safes bound the wait when something is wrong
rather than merely slow, and all three fall back to the socket close (an
ORA-03113 that is *meant*) after finalizing the statement with the real reason:

| Fail-safe | Fires when | Bound |
|---|---|---|
| `refusalHoldMaxBytes` | the reply never reaches a boundary at all | 8 MiB relayed past the violation |
| `refusalHandoffGrace` | the client stops talking with a refusal undelivered | 30 s, enforced by `onLimitViolation` |
| a call dbbat cannot name | the next op is an unwalkable piggyback | immediate close, no frame (`answerHeldRefusal`) |

The grace is why `onLimitViolation` had to learn to stand down: `LimitGuard.Watch`
fires its hook **once** and returns, and the violation stays true the whole time
the refusal is held, so without the wait the watchdog would drop both sockets a
poll interval later and the client would meet the same ORA-03113 the hold exists
to replace. Standing down is bounded, never unconditional — a handoff that never
happens is exactly what the watchdog is the fail-safe for.

Nothing is under-enforced by the wait: every client **Data** packet that arrives
while a refusal is held ends the session instead of being forwarded, so the only
extra data that crosses is the reply already in flight; and the statement is
logged with `aborted: bandwidth quota exceeded for this grant` on every one of
the four paths (delivered, unnameable, over-bytes, over-grace).

Data packets are the exact bound, not a hedge. `clientToUpstream` routes only
TNS Data packets of at least `ttcDataFlagsSize+1` bytes into
`interceptClientMessage`; anything else — a **Marker** (type 5: the break /
attention packet a client sends to cancel a call), a Control packet — is relayed
upstream unread, refusal held or not. That is deliberate rather than overlooked:
none of those carry a statement, so nothing can be executed through them, and
what they *do* provoke from the upstream is bounded by `refusalHoldMaxBytes` like
any other relayed reply. Blocking a break would also be actively wrong — it is
how a client asks to stop, which is the same thing dbbat is trying to do.

Within Data packets the guarantee is unconditional, and that took work.
`interceptClientMessage` is built to **fail open** — a frame dbbat cannot parse,
or a decode that panics, is forwarded unread, because refusing a frame it could
not identify protects nothing and strands the client (the argument is spelled out
at length on `gateUnnameableFrame`). Under a held refusal that reasoning inverts:
the grant is exhausted and the session is over, so an unreadable frame would be
the one way a client message travels past an exhausted grant. Both unreadable
exits and the panic recovery therefore route through `heldRefusalBlocks`, which
forwards as before when no refusal is held and gives the unnameable-call answer
when one is — sockets dropped, no frame, since an OER stamped with a stale number
ends a call the client is not parked on. Of its three callers only the recovered
panic is live traffic: `clientToUpstream`'s own length gate means the two
early returns cannot fire from there, and they are guarded so the guarantee
belongs to the function rather than to a check one caller happens to perform.

Two smaller invariants hold the same line. The answer is written **once** — the
client leg keeps reading until its socket really closes, and a second OER would
be exactly the unsolicited frame `onLimitViolation` refuses to send. And the
teardown is **deferred rather than sequential**, so a panic inside the frame
write is not a fifth, panic-shaped exit that skips the recording and leaves both
sockets open: it still finalizes the statement, still closes both legs, and — via
a nested recover in `heldRefusalBlocks` — still does not escape, which matters
because `proxyMessages` starts the relay goroutines bare and the only recover
above them is on a different goroutine.

Measured after the fix, same fixture, same tap: dbbat still writes **exactly one**
OER, with a non-zero call number, and **ojdbc now reports it** —
`midfetch: code=28`, where it used to say `code=3113` — after draining 1500 of
5000 rows, with no ORA-18745 anywhere and no watchdog teardown.

**go-ora cannot confirm it, and that is the driver's doing, not dbbat's.** It
still answers `driver: bad connection`, and the reason is now known: go-ora maps
ORA-00028 to a dead connection *on purpose*. `network.OracleError.Bad()` lists 28
next to 3113/3114/12537, and `defaultStmt.fetch` turns any error `isBadConn`
accepts into `driver.ErrBadConn`, so a mid-result-set ORA-00028 reaches the
caller through `database/sql` as "bad connection" whether it was parsed or never
arrived. The error text cannot separate those two, so the sibling subtest stopped
asking it to: go-ora now runs through **its own recording tap**, and the
assertion is that dbbat wrote exactly one well-formed ORA-00028 there (call
number 12, against ojdbc's 8, after 1580 of 5000 rows), held first and delivered
on the client's next call (`logMsgRefusalHeld` then `logMsgRefusalDelivered`).
Which is *stronger* evidence than the old text assertion, and it is what makes
ojdbc's `code=28` attributable to the fix rather than to ojdbc.

Confidence: the enumeration is read off the code and pinned by
`TestUpstreamToClient_ByteLimitHoldsRatherThanCuttingIn`,
`TestHeldRefusalEndsTheCallTheClientIsNextParkedOn`,
`TestHeldRefusalMeetingAnUnnameableCallClosesInstead`,
`TestHeldRefusalBlocksAFrameItCannotRead`,
`TestHeldRefusalIsAnsweredExactlyOnce`,
`TestHeldRefusalTearsDownEvenWhenTheFrameWriteBlowsUp`,
`TestHeldRefusalStandsTheWatchdogDownUntilItsGrace`,
`TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking`,
`TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed` and
`TestIdleLimitViolationSendsNoOER`; the live cases above are measured against
Oracle 23ai Free and pinned by the integration tests named with them. The driver
they were taken on is **ojdbc 23.7.0.25.01**, not the 26.1 the call-number
finding is attributed to; what that does and does not license is in "Which ojdbc
these results are attributed to" above.

Two things are *not* measured, and the asymmetry is worth stating rather than
leaving to be discovered. The **fail-safes have never fired end to end**:
`TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed` reaches
`refusalHoldMaxBytes` by subtracting it from the held refusal's own byte mark,
and `TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking` reaches
`refusalHandoffGrace` by backdating its arming time, since no live client
produces a reply with no boundary or an 8 MiB overshoot on demand. And the
delivered path is measured on **two of four clients** — sqlplus (OCI) and
python-oracledb have not seen a held refusal at all, which matters most for the
unnameable fallback, since an OCI session's own frames are the ones dbbat
routinely cannot name. Both are
`specs/todos/2026-08-13-07-measure-the-held-mid-reply-refusal-on-the-other-two-clients.md`.

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
- **The `0x11` fetch reading is unreachable, and that is the honest state**: `handleOFETCH` gates a fetch that starts a fresh pending query as a re-execution, but message type `0x11` is the piggyback message type and no client sends a fetch that way — real fetches are `03/05`, which dbbat does not intercept. The reading was only ever reached by misparsing piggybacks, which is the bug written up under "Two OCI encodings, not one"; the gate itself is still pinned by unit tests and the other two re-execution frames (the SQL-less `OALL8` and the `03/0x4e|0x04` piggyback) are real and enforced. Wiring the gate to `03/05` is a behaviour change on the hot path and is filed as its own todo.
- **Row capture is best-effort**: The TTC binary format varies across Oracle client versions. Some clients/query types may produce partial or no row capture. SQL text extraction works reliably across all tested clients.
- **Column names**: Real column names come from the describe column-definition records (`parseColumnDescribes` in `describe.go`), so single-char aliases (`SELECT level AS n`) and unnamed expressions (`SELECT count(*)`) get their true names and positions. Only genuinely unnamed expression columns fall back to a synthetic `COLn` label. If the records don't parse on some server layout, decoding falls back to heuristic name-scanning plus describe-header count padding, so the column count (and row framing) stays correct.
- **DML row counts**: INSERT/UPDATE/DELETE affected-row counts are captured from the v315+ OER status block (TTC func `0x04`, embedded in the execute Response) and stored as `rows_affected`, for clients whose OERs carry the end-of-call bit and (since the fix above) for those whose don't. **Failed statements record their ORA error text on every client**, out of the *standalone* func `0x04` that is how failures actually arrive — see the measurement under "the OER end-of-call bit is not universal", which found the bit to be a property of the call rather than of the client. The one gap left is a failure raised **mid-fetch**, once column definitions are decoded: there a `0x04` run is row data by definition, the strict decoder stays the only thing allowed to end the call, and such a failure is therefore still recorded with no error at all. Never measured; filed as `specs/todos/2026-08-13-09-oracle-mid-fetch-failure-records-no-error.md`. See `ttc_oer.go`.
- **Bind values (parameterized queries)**: Bind values are captured from both the legacy `OALL8` execute path (`decodeBindValues`) and the v315+ **piggyback exec** path that modern clients use (`extractPiggybackBinds`, func `0x03` sub `0x5e`). The piggyback binds sit length-prefixed at the tail of the message; they're located as the suffix that parses as exactly as many values as there are distinct bind placeholders in the SQL, and each is decoded by content via `decodeOracleRawValue` (so a NUMBER bind like `42` renders as `42`, not hex). Verified against `testdata/go_ora_binds.pcapng` (`TestDumpReplay_Binds`). Captured binds are now persisted to `queries.parameters` (`formatOracleBinds` wired into `persistQueryRecord` and `completeQuery`), so the API (`GET /api/v1/queries/:uid`) and the UI Parameters card report them. Not yet handled: binds over ~253 bytes (extended length encoding) and full type-aware decoding from the bind-definition records.
- **Temporal types**: DATE, TIMESTAMP, and TIMESTAMP WITH TIME ZONE decode in captured results, verified end-to-end against `testdata/go_ora_temporal.pcapng` (`TestDumpReplay_Temporal`). The tz form renders the local wall clock plus its numeric offset, honouring byte 11's `0x40` "time in zone" flag (prefix stored as local vs UTC). Named-region time zones fall back to the stored wall clock without an offset suffix.
- **Large result sets**: The QueryResult (func `0x10`) row area and continuation packets (func `0x06`) share one decoder (`parseRowStream`) that walks the full compressed row stream — length-prefixed values plus the `0x15 [flag] [count] [bitmask] 0x07` column-compression descriptors between rows. A 400-row single-packet result is captured end-to-end against a live-Oracle ground-truth fixture (`testdata/go_ora_largeresult.pcapng`, `TestDumpReplay_LargeResultRows`). Multi-TNS-packet (small-SDU/JDBC) result sets reuse the same decoder via the continuation path; their per-row correctness is not yet ground-truth-verified.

## Testing

**Per-client verdicts live in exactly one place: "Client compatibility on Oracle
23ai" below.** That is the table that is kept current, that names the test
behind each verdict, and that carries the DBeaver / ojdbc notes. There used to
be a second table here; it was a snapshot from before the 23ai auth work landed
and it disagreed with the real one on every client that mattered (sqlplus /
OCI, python-oracledb thin and SQLcl were all listed as failing at AUTH, and all
three now work), so it is gone rather than duplicated.

The short version: all four supported client families — go-ora,
python-oracledb thin, SQLcl/ojdbc and sqlplus / OCI instant client —
authenticate, query and capture end-to-end against Oracle 23ai through the
proxy. Against Oracle 19c the historical behaviour still applies.

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
| `ORACLE_TEST_OJDBC_JAR` | Oracle JDBC driver jar for the JDBC-thin refusal case; without it (and without an `ojdbc*.jar` on `CLASSPATH`) that one test skips |
| `ORACLE_TEST_REQUIRE_OCI_CLIENT` | `1` turns "no OCI client available" from a skip into a failure. Set on every Oracle leg in CI — see below |
| `ORACLE_TEST_OCI_CLIENT` | Pins where sqlplus comes from: `path` (an install on `PATH`) or `container` (the one bundled in the Oracle image). Unset = auto, `PATH` first |

#### Where the OCI client comes from

The sqlplus-driven tests — `TestIntegration_SqlplusLoginThroughSyntheticAuth`
and `TestIntegration_BlockedStatementRefusesSQLPlus` — are the only end-to-end
proof that an OCI client can authenticate through dbbat at all, and the only
automated cover for the unlearned fallback in `nextOERFrame`. They used to open
with `exec.LookPath("sqlplus")` and skip when it was absent, which was every CI
runner: green by proving nothing.

They now take a client from one of two places, decided once per process in
`oci_client_integration_test.go` and *before any container starts* (the second
option is a container- and listener-creation-time decision the fixture cannot
revisit):

1. **sqlplus on `PATH`** — an Instant Client install. Preferred when present: it
   costs nothing, and it is the flavor `testdata/sqlplus_cursor_reexec.pcapng`
   was captured from.
2. **the sqlplus bundled in the Oracle container the suite already started**,
   run over `docker exec`. gvenzl's images all ship one — 23.26 in
   `oracle-free:23-slim`, 18.4 in `oracle-xe:18.4.0-slim` — so CI needs no
   Instant Client zip, no download and no cache.

Route (2) needs two things the ordinary fixture does not do, so they are opt-in
per test (`startOracleThroughProxyForOCI`) rather than global: the proxy binds
`0.0.0.0` instead of loopback, and the Oracle container is created with a
`host.docker.internal:host-gateway` entry so it can dial back. Every in-process
client still connects to `127.0.0.1`.

Route (2) is also a *different* OCI flavor from route (1), and that is a second
reason to prefer it in CI: the DB-bundled client sends plain key/value length
fields where the Instant Client sends 3x UTF-8 buffer sizes (delta 3 in the
table under "The fallback covers wide/OCI clients too"). Only the Instant
Client shape had live coverage before; the bundled one existed as a captured
cross-check.

Auto-selection means a machine with an Instant Client installed never exercises
route (2) — the only route CI has. `ORACLE_TEST_OCI_CLIENT=container` pins it,
which is how the container route is reproduced locally:

```bash
ORACLE_TEST_REQUIRE_OCI_CLIENT=1 ORACLE_TEST_OCI_CLIENT=container \
  go test -race -tags integration -timeout 40m -count=1 -v \
  -run 'TestIntegration_SqlplusLoginThroughSyntheticAuth|TestIntegration_BlockedStatementRefusesSQLPlus' \
  ./internal/proxy/oracle/
```

Two probes run before the client is accepted, and each maps to a different
verdict — a missing or unroutable *client* is an environment fact, while a
client that runs and then fails to log in is a finding about the proxy and must
reach the assertions as a failure:

- `sqlplus -v` inside the container, and
- a TCP connect from the container back to the proxy's port.

One test is **not** yet run on route (2), and the reason is a measured defect
rather than a harness limit: under a restrictive grant the bundled 23.26 client's
first call in proxy mode is refused as an untracked cursor re-execution
(`cursor_id=27396` on a session that has executed nothing) and the session then
hangs. `TestIntegration_BlockedStatementRefusesSQLPlus` skips on that flavor
naming
`specs/todos/2026-08-12-12-bundled-oci-client-refused-and-hung-under-a-restrictive-grant.md`;
the login test holds an unrestricted grant, fires no gate, and runs on it. The
Instant Client on PATH passes both against the same upstream — which is what
makes this a statement about that client flavor and not about the gate.

When neither route yields a client the tests skip, **unless**
`ORACLE_TEST_REQUIRE_OCI_CLIENT=1`, which turns that into a failure.
`.github/workflows/integration.yml` sets it on all three Oracle legs, so the
OCI login is proved on both Oracle versions and in both AUTH modes (legs 1 and
2 exercise the wide AUTH *rewrite*, leg 3 the wide *synthetic* builders). A
red 18c leg on these two tests is therefore a real statement about that OCI
vintage against dbbat, not a harness gap.

#### The suite runs under `-race`, and why that matters here

`make test-e2e-oracle` passes `-race`, as does the CI job. It is not
belt-and-braces: this suite is the only thing that puts the proxy's *two*
session goroutines — the client reader and the upstream reader, plus the limit
watchdog and the approval gate — on the same session at once. The unit tests
carry the detector but drive a single goroutine, so every shared-state bug in
`session.go` / `intercept.go` was invisible to both halves of the test suite at
the same time: the tests that could see it had no detector, and the tests that
had the detector could not reach it.

It found three, all the same shape — the client goroutine and the upstream
goroutine writing one session's bookkeeping with nothing between them:
`session.oer` (the OER shape learned from the pre-auth relay, a
`min(existing, observed)` read-modify-write from both sides — found by review,
not by the suite), and then `tracker.pendingQuery`, written by
`handlePiggybackExec` / `completeQuery` on the client leg while
`upstreamToClient` read it once per forwarded packet to decide whether the
mid-stream limit check applied. `completeQuery` is also a read-modify-write of
`lastBytesSnapshot` and of the grant's in-session byte counter, which is what a
cumulative byte quota is enforced against.

The fix is `trackerMu` (and `oerMu` before it): per-concern locks, not a
session-wide one. The upstream leg takes `trackerMu` once for the whole of
`interceptUpstreamMessage`, which does no socket I/O; the client leg takes it in
explicit `book()` steps, with `holdIfNeeded` deliberately between two of them
rather than inside one — an approval hold has no timeout, and holding the lock
across one would park the response leg on a human.

The cost was measured rather than assumed, on this target, back to back:

| | wall | test time | user CPU |
|---|---|---|---|
| without `-race` | 6m38s | 392s | 8.7s |
| with `-race` | 6m16s | 371s | 14.6s |

The detector is the expected ~2x CPU tax, but on ~6 seconds of CPU in a suite
that spends six minutes booting Oracle containers — so it disappears into the
run-to-run noise, and `-timeout 40m` needed no change.

#### How a refusal ends the client's call

`TestIntegration_BlockedStatementsAreLogged` drives a real go-ora client through
the proxy under a `read_only` and then a `block_ddl` grant;
`TestIntegration_BlockedStatementRefusesPythonThin`,
`TestIntegration_BlockedStatementRefusesSQLPlus` and
`TestIntegration_BlockedStatementRefusesJDBCThin` do the same from
python-oracledb thin, sqlplus (OCI) and JDBC thin. The python and JDBC ones skip
when that client is not installed; the sqlplus one falls back to the client
bundled in the Oracle container (see "Where the OCI client comes from") and
skips only when even that is unusable — never in CI, where
`ORACLE_TEST_REQUIRE_OCI_CLIENT=1` makes it a failure. Each refusal is enforced
(nothing reaches upstream),
logged (a `queries` row carrying the refusal as `error`, an ordinary link in the
connection's HMAC chain), returned to the client as ORA-01031, and leaves the
connection usable for the next statement.

That last part was broken for a long time and no test noticed, because the only
assertion was on the `queries` row. `session.writeTTCError` emitted a TTC
Response (0x08) with fixed-width big-endian fields where a server ends a call
with an OER (0x04), and framed it with the legacy 2-byte TNS length on top — so
a v315 client read the length bytes as a multi-megabyte packet and blocked
forever. Measured against Oracle 23ai Free, go-ora's `ExecContext` never
returned. See "Ending a call: the OER encoder" above.

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

The Set Protocol response capability array is framed `[numCaps][caps…]`, where `numCaps`
varies by server version (`0x2a` on 19c, `0x36` on 23ai) and the array **opens with** the
byte run `06 01 01 01` — that run is `ServerCompileTimeCaps[0..3]`, not a prefix sitting in
front of the array. Reading it as a prefix and indexing from the byte after it shifts every
capability by four, which is invisible at index 0 and wrong everywhere else; both
`observeCustomHashFlag` and `observeBigClrChunksFlag` used to do exactly that. They now go
through `serverCapBitSet`, which locates the array with `serverCompileTimeCaps` (the same
preamble walk go-ora's `TCPNego.read` does) and indexes it as Oracle does:

| capability | index | 19c (`numCaps` 0x2a) | 23ai (`numCaps` 0x36) | verdict |
|---|---|---|---|---|
| `customHash` (PBKDF2 combined key) | `caps[4]&0x20` | `0x6f` | `0xef` | set on both |
| `UseBigClrChunks` | `caps[37]&0x20` | `0x7f` | `0x7f` | set on both |
| *(what the shifted anchor used to read for `UseBigClrChunks`)* | `caps[41]` | `0x0d` | `0x0d` | clear — the bug |

So dbbat used to conclude `UseBigClrChunks = false` on every session where go-ora, JDBC thin
and python-oracledb all conclude `true`. `customHash` was right by accident: offset 0 of the
shifted array *is* the real `caps[4]`. The flags are measured off the captures in
`internal/proxy/oracle/testdata/` and pinned by
`TestServerCapBitSet_RealSetProtocolReplies`.

**Every AUTH site now follows the negotiated flag — there is no remaining hard-coded
dialect.** Once the capability was read correctly it had to reach each place that frames or
decodes a CLR value, which took four passes; the inventory, so a future addition can be
checked against it:

| site | function | side |
|---|---|---|
| Phase 2 fallback rewrite | `rewritePhase2KVPairs`, `rewriteAuthPhase2`, `replaceAuthKVValue` | both |
| upstream challenge parse | `parseAuthKVDictionary`, `readAuthKVPair` | read |
| client-facing challenge | `buildAuthChallenge` | write |
| wide/OCI leg | `ttcKeyValWideChunked`, `ttcKeyValWideSized`, `replaceAuthKVValueWide`, `readAuthKVPairWide`, `buildClientAuthPhase{1,2}Wide` | both |
| thin synthetic upstream body | `buildClientAuthPhase1`, `buildClientAuthPhase2` | write |
| client Phase 2 finders | `parseAuthPhase2`, `scanTTCKeyValPairs` / `readKVValue`, `findKVByKeyBytes`, `findKVByKeyBytesWide` | read |

The primitives are `ttcClrVariant` / `ttcKeyValChunked` on the write side and
`readCLRVariant` on the read side. Below 252 bytes the two encodings are byte-identical, so
none of this moves a byte for the values dbbat handles today —
`TestThinSyntheticAuthLeg_ShortValues_ByteIdentical` and
`TestWideAuthLeg_ShortValues_ByteIdentical` pin that on the synthetic bodies. It becomes
live the day a value outgrows the short form, which for the synthetic bodies most plausibly
means a long hostname in `AUTH_TERMINAL` / `AUTH_MACHINE` (no Oracle-side cap) or a grown
`AUTH_PBKDF2_SPEEDY_KEY`.

**The one deliberate exception is the username**, which stays on plain `ttcClr` in both
synthetic builders: Oracle caps an identifier at 128 bytes, half the short-form limit, so it
can never reach the `0xFE` long form and the two encodings cannot disagree about it.

The finders in the last row extract only `AUTH_SESSKEY` (64 hex chars) and `AUTH_PASSWORD`
(96/64) for `parseAuthPhase2` itself — fixed-size by construction, so the flag could have
been argued away there. It is threaded anyway for two reasons: `scanTTCKeyValPairs` is not
selective and decodes *every* `AUTH_` pair it crosses on the way, including a client's
`AUTH_CONNECT_STRING` (routinely past 252 bytes with a load-balancer hostname); and
`clientDeclaredProgramName` reads `AUTH_PROGRAM_NM` through the very same finders, which is
free-form client text with no length cap.

`customHash` is relayed to the client unchanged, so a modern client negotiates it and dbbat
answers with a verifier-18453 challenge; legacy go-ora reads the verifier type from the
challenge's `AUTH_VFR_DATA` flag and falls back to 6949.

### Client compatibility on Oracle 23ai

Verified end-to-end (authenticate + query + observability capture) against Oracle 23ai
(`23.26`) through the cluster proxy from the Windows host:

| Client | Protocol | Status | Notes |
|--------|----------|--------|-------|
| go-ora | thin | ✅ works | accepts 6949 or 18453 |
| python-oracledb thin | thin | ✅ works | FAST_AUTH de-pipelined; verifier 18453 |
| SQLcl 26.1.2 (ojdbc) | thin | ✅ works | classic O5LOGON; verifier 18453 |
| sqlplus / OCI instant client | thick | ✅ works | auth + query work via the **wide** (4-byte LE) TTC encoding, with **no dependency on OOB/`DISABLE_OOB`** — verified locally against Oracle 23ai and through an OOB-stripping TCP relay (a NodePort/NLB stand-in). See "OCI wide encoding" and "OCI break/reset before AUTH Phase 2" below |

Go (`go-ora`) additionally has **bind values** captured end-to-end against
Oracle 23ai Free, which no other client family is ground-truth-verified for yet.

Two more clients are exercised outside that campaign, so they sit next to the
table rather than in it:

- **ojdbc11 driven directly (JDBC thin)** — statement *refusals* are verified
  end-to-end against Oracle 23ai Free on ojdbc11 23.2.0.0, 26.1 and
  23.7.0.25.01, by `TestIntegration_BlockedStatementRefusesJDBCThin`. That test
  takes whatever `ORACLE_TEST_OJDBC_JAR` points at and asserts nothing about the
  version, so the versions above are what the trees were *observed* to carry —
  see "Which ojdbc these results are attributed to". Row capture on ojdbc was
  only ever **partial** in the older tests; the modern column-describe path
  ("SQLcl/ojdbc result capture" below) is what improved it, and it has not been
  re-measured across ojdbc versions.
- **DBeaver** (JDBC thin via ojdbc) — connects and its SQL is logged; row
  capture **partial**, from those same older manual runs. DBeaver is not part of
  the automated suite, so nothing in CI would notice if that regressed.

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

**The wide leg follows `UseBigClrChunks` too.** The wide encoders and readers used to drop
the negotiated CLR long form on the claim that "OCI does not use it". That claim was an
assertion, never a measurement, and the capture it would have to rest on does not support
it — `TestSqlplusCapture_NegotiatesBigClrChunksAndCarriesNoLongValue` reads
`testdata/sqlplus_cursor_reexec.pcapng` and finds:

- the OCI session **does** negotiate the capability: `ServerCompileTimeCaps[37]` is `0x7f`,
  the same value every thin session sees, because the capability is advertised by the
  *server* and has nothing to do with the client's dialect. `session.clientBigClrChunks` is
  therefore true on an sqlplus login, and the wide leg is genuinely reachable with the flag
  set;
- but **no** AUTH value in that login reaches the `0xFE` long form — the longest is
  `AUTH_CONNECT_STRING` at 172 bytes, under the 252-byte short-form limit where the two
  encodings are byte-identical. Read with either flag, the captured body decodes the same.

So the capture cannot say which chunk-length encoding OCI writes past the limit; the
negotiated capability is the only evidence there is, and the leg now follows it —
`readCLRVariant` on the read side (`readAuthKVPairWide`, `parseAuthKVDictionary`,
`replaceAuthKVValueWide`), `ttcClrVariant` on the write side (`ttcKeyValWideChunked`,
`ttcKeyValWideSized`, the wide branch of `buildAuthChallenge`). Below 252 bytes — every
value dbbat writes or substitutes today — it moves no byte, which
`TestWideAuthLeg_ShortValues_ByteIdentical` pins across all four write sites. What changes
is only what happens the day a verifier, a salt or a spliced key outgrows the short form:
the wide leg then frames it the way the session negotiated instead of the other way.

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

#### Two OCI encodings, not one (proxy mode)

Everything above is about AUTH. The same split runs through **proxy mode**, and it was found
the hard way: wiring the DB-bundled client (sqlplus 23.26, inside `gvenzl/oracle-free:23-slim`)
into CI turned up three defects in a row, each of which hung a session under a `read_only`
grant while the Instant Client 23.3 sailed through. Same protocol version, same upstream,
same statements — the difference is that the bundled client marshals TTC at **64-bit**
widths where the Instant Client uses 32-bit ones.

**1. `0x11` is the piggyback message type, not a fetch.** dbbat's `TTCFuncOFETCH = 0x11`
is a misnomer kept for continuity: in TTC, `0x11` opens a *piggyback* message and byte 1 is
the TTC **function code** — `0x69` close-cursors, `0x6b` an OCI session piggyback, `0x87`
set-end-to-end-attrs, `0x98` set-schema. A real fetch is message type `0x03`, function
`0x05` (`TNS_FUNC_FETCH`), which is what every recording in `testdata/` carries.
`decodeOFETCH` reads a big-endian cursor id out of bytes 1..3, so on the bundled client's
very first message — `11 6b 04 …` — it read (function, sequence) and produced cursor id
`0x6b04` = 27396, which no session had ever opened. Under any statement-shaped control that
is a refusal, on the first call of every session. The Instant Client sends the same frame
and was refused identically; it just shrugs the refusal off, which is why this went
unnoticed for as long as the PATH flavor was the only one running.

**2. dbbat refuses only a call it can name.** A piggyback is by definition in front of
something, and the call the client is parked on is stapled *behind* it. dbbat can walk one
piggyback body — the close-cursors list — and for anything else the sequence number at
offset 2 is the piggyback's own, not the call's. `clientCallNumber` now reports that it
cannot name such a message, `observeClientCallNumber` leaves the last known-good number
alone rather than overwriting it with a wrong one, and `interceptClientMessage` routes it
to `gateUnnameableFrame` instead of to either reading that ends in an OER. **Failing open
is deliberate** — a message dbbat cannot name is one it could not identify either, so
refusing it protects nothing, while answering it ends a call the client is not waiting for
and parks it forever.

The check runs **before** the exec reading, not after, and that ordering is load-bearing: a
`11 69` execute whose close list does not walk is exactly as unnameable as any other
piggyback, and gating it while stamping the last-seen sequence number is the ORA-18745 /
hang mode of `specs/done/2026/08/2026-08-12-02-oracle-async-refusal-call-number.md`. Every
recorded `11 69` from a real client walks (54 in `dbeaver.pcapng`), so it was never
live-visible — which is precisely why it is pinned by a test rather than left to chance.

**Fail-open is bounded, and the bound is the point.** A piggyback is a frame with something
stapled behind it, so forwarding one unread would let an authenticated user under a
`read_only` grant put an `INSERT` behind a body dbbat cannot walk and have it travel
ungated — a worse outcome than the hang. `gateUnnameableFrame` therefore scans the frame
for a stapled statement with the same extractor the JDBC exec path gates on
(`decodeExecSQL`) and runs it through the same pre-flight in the same order: the quota
check, then the grant's static controls, its approval patterns, **and the validators that
fire with no controls at all** — the Oracle blocked patterns (`ALTER SYSTEM`, `UTL_HTTP`,
`DBMS_SCHEDULER`…) and the password-change guard. Skipping those would have made an
unwalkable piggyback the one place `ALTER SYSTEM KILL SESSION` travels under an ordinary
full-access grant while the identical statement in a nameable frame is refused. A statement
the pre-flight permits travels; one it refuses does not.

**And an allowed statement is recorded**, with the same `persistQueryRecord` the JDBC path
uses. That is not bookkeeping: "every query logged" is the premise, so a path that forwarded
a statement while writing nothing would make a client dialect whose close list never walks
the one place a session's SQL escapes the audit trail entirely. It is also what makes
`max_query_counts` apply here — the quota is charged when a pending query completes on the
response leg, so a statement nobody tracks is a statement nobody counts. Revocation, expiry
and the byte quota were never at risk on this path: `LimitGuard` catches those on the
response leg regardless of what the client leg decided.

**The scan is anchored, and that is a bound rather than a weakening.** A refusal here ends
the session, and the frames that reach it are the ones dbbat cannot parse — including
piggybacks that carry caller-supplied text by design, like `11 87` set-end-to-end-attrs
with its module, action and client-identifier strings. A bare keyword scan would kill the
session of any client that set its module to "DELETE ORDERS". So the statement is only read
from the start of a TTC op that can carry one (`03 5e`, `11 69`, `11 98` —
`statementOpOffsets`). Bytes with no such header in front of them are bytes the *upstream*
will not execute either, so anchoring lets nothing runnable through: hiding a statement
from dbbat while keeping it executable by Oracle is exactly what it refuses to allow.

**One carve-out, deliberate.** `statementOpOffsets` counts offset 0, so a frame whose *own*
header is a statement op — a `11 69` / `11 98` execute that reached this path only because
its close list did not walk — anchors at 0 and is scanned whole, which is the unanchored
behaviour again. It has to be: such a frame really is an execute and its SQL really is in
it, so declining to look would forward a live statement ungated. The price is that a `11 69`
frame whose stapled set-end-to-end-attrs strings read as a refused statement ends the
session. That is fail-closed on a shape no tested client produces — all 54 recorded `11 69`
frames in `dbeaver.pcapng` walk, as do both bundled-client ones — against fail-open on a
live exec, and whether the shape occurs at all is filed for measurement.

The extractor itself is weaker than the gate built on it — it returns the first hit rather
than the executable one, and its keyword fallback omits `TRUNCATE`/`GRANT`/`REVOKE`. That
is a **pre-existing property of both this path and the ordinary JDBC exec path**, not
something anchoring introduced, and it is filed as
`specs/todos/2026-08-13-05-oracle-sql-extraction-is-weaker-than-the-gate-that-uses-it.md`
rather than patched in passing, because widening a keyword scan trades directly against the
false positives above and needs measuring first.

Refusal there is by **ending the session**, not by an OER, and for the same reason
`onLimitViolation` drops the socket rather than writing a frame: there is no call to end.
The statement is recorded as blocked first, so it is in the audit trail like every other
refusal, and the client sees a plain I/O error rather than a wait that never returns —
which is what a real Oracle does on `DISCONNECT SESSION`. In a live bundled-client session
both unnameable frames (`11/6b` at login, `11/c0` at logoff) report `carries_statement=false`
and are forwarded untouched, so this path costs an ordinary session nothing.

**3. The close-cursors list has a 64-bit header too.** The bundled client writes
`11 69 <seq> 00 00 <ub4> <sb8 seq+1>` — a 17-byte op header — where the Instant Client
writes `11 69 <seq> 01 <seq+1>`, and its count is an 8-byte field ahead of the same 4-byte
ids (`isCloseCursorsWide8Header` / `decodeCloseCursorsWide8`, pinned on
`testdata/oci_bundled_close_cursors.hex`). Until dbbat could walk it, the *execute stapled
behind the list* was invisible: a refused `INSERT` went out carrying whatever sequence
number dbbat had last seen instead of the call's, and sqlplus waited for the right one
forever.

**4. The summary object has a 64-bit layout.** Same fields, same order, wider offsets:
call status `u32@1`, ECID `u16@5`, error number `u16@12`, cursor id `u16@18`, call number
`u32@49`, RetCode `u32@132`, a 136-byte prefix, then an 8-byte row count and the tail
fields (`oerFixed64Layout`, measured against two real ORA-01403 summaries in
`testdata/oci_bundled_oer.hex`). `learnOERShape` recognized neither of them before, so the
session stayed on the unlearned default and dbbat answered a 64-bit client with a 32-bit
frame — a third hang, on the refusal itself. The learner now tries both layouts, widest
first, and each is validated by the invariant it already used: the trailing RetCode must
repeat the leading error number *at that layout's offsets*, and the message must end
exactly where the tail-field walk says it does.

The **unlearned** fallback is covered too, and it had to be: `learnOERShape` reads the
upstream's OERs, so a session that must refuse before one has arrived — an approval pattern
matching the opening statement, an already-exhausted quota, a first statement that is a
write under `read_only` — has nothing learned to go on. `nextOERFrame` therefore seeds
`fixedWidth64` from the client's own AUTH Phase 1, which opens with the same 64-bit op
header (`03 76 02 00 00 <ub4> <sb8 seq+1> fe…`, against the Instant Client's
`03 76 02 01 03 fe…`) — see `usesWide64OpHeader` and
`testdata/oci_bundled_auth_phase1.hex`. No integration test can reach that window, because
sqlplus issues its own login SELECTs before anything a grant would refuse, so it is pinned
by unit tests on both dialects.

`logMsgLearnedOERTail` reports `fixed_width_64` alongside `fixed_width`: on a hung OCI
client, `fixed_width=true fixed_width_64=false` against a 64-bit client is the whole bug,
and nothing else in the log distinguishes the two layouts.

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

### The AUTH function header is negotiated, not fixed

Every TTC function call opens with `[03 <sub> <seq>]` — piggyback marker, sub-op
(`0x76` Phase 1, `0x73` Phase 2) and a per-session function sequence number
(1 for Phase 1, 2 for Phase 2). Once the negotiated TTC field version reaches
18, **one extra `0x00` follows** (go-ora's `PutTTCFunc`; the version is the min
of the client's and the server's `CompileTimeCaps[7]`). Both widths are on the
wire in `internal/proxy/oracle/testdata`:

| capture | Phase 1 | Phase 2 | framing |
|---|---|---|---|
| `go_ora_cursor_reexec.pcapng` (go-ora v3, 23ai) | `03 76 01 00` | `03 73 02 00` | thin, extended |
| `jdbc_thin_cursor_reexec.pcapng` | `03 76 01 00` | `03 73 02 00` | thin, extended |
| `python_thin_cursor_reexec.pcapng` | `03 76 01` | `03 73 02 00` | thin, mixed |
| `python_thin.pcapng` | `03 76 01` | `03 73 02` | thin, narrow |
| `dbeaver.pcapng` | `03 76 01` | `03 73 02` | thin, narrow |
| `go_ora.pcapng` (go-ora v2 era) | `03 76 00` | `03 73 00` | thin, narrow, sequence hard-coded to 0 |
| `sqlplus_cursor_reexec.pcapng` | `03 76 02` … | `03 73 03` … | **wide (OCI)** — width not inferable, see below |

Anything that reads or writes an AUTH preamble must therefore get the width from
`ttcAuthFuncHeaderLen` rather than assume one. Getting it wrong shifts every
field after the header by a byte, and the upstream answers two break markers plus
`ORA-03120` — indistinguishable, from the outside, from the caps mismatch above.

The discriminator is byte 3, **and only on a thin (compressed-encoding) body**:
there the header is always followed by the `0x01` username-present marker, so a
`0x00` in that position can only be the extension byte. A wide (OCI/sqlplus)
body frames its preamble as pointer runs and 4-byte little-endian fields with no
such marker, so byte 3 says nothing at all — `ttcAuthFuncHeaderLen` returns
`ok=false` for those and for anything too short to tell. `ok=false` carries the
narrow width, which is the width every reader here assumed before any of this
was understood; a caller that *writes* a header must not narrow on it.

Fixed 2026-08-10 in four places: the synthetic `buildClientAuthPhase1` /
`buildClientAuthPhase2` (which had hard-coded go-ora **v2**'s `03 76 00 01` and
so could never have logged in against a modern negotiation), the fixed-offset
parsers behind the anchored rewrite in `phase1_forward.go` /
`phase2_forward.go`, and the client-side username parse in `parseAuthPhase1`
(`ttc_auth.go`). Every one of them sat behind an anchored path that reads the
`AUTH_*` keys instead of the preamble, which is why nothing was visibly broken.

### Exercising the synthetic AUTH fallback

`sendUpstreamAuthPhase1` / `sendUpstreamAuthPhase2` forward the client's own
packet with the username swapped whenever they can, which in practice is always;
the synthetic builders are the fallback for a missing or unrewritable client
packet. That fallback therefore never runs on an ordinary integration run, and
it rotted unnoticed for months — a green integration suite proved nothing
about it.

`.github/workflows/integration.yml` now runs a third Oracle leg
(`23ai Free (synthetic AUTH fallback)`) with the variable below set, alongside
the two ordinary legs, on every scheduled/dispatched run of that workflow —
so this can't silently rot again.

Set `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1` to disable the rewrite for a whole
integration run and drive the synthetic path against a real Oracle instead:

```bash
DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1 go test -tags integration -v -timeout 20m \
  -count=1 -run TestIntegration_MCPExecutesThroughTheProxy ./internal/proxy/oracle/
```

The flag is read once, by the integration suite's `TestMain`; nothing in
production writes it.

Both runs — with and without the variable — must pass.

#### The fallback covers wide/OCI clients too

The synthetic builders used to emit the thin (compressed) KV encoding and a
go-ora-shaped KV set only, so a wide/OCI session (sqlplus, Instant Client)
whose rewrite failed was handed a body its caps-conditioned upstream could not
parse. That is no longer true: `upstream_auth_client_wide.go` is the OCI
counterpart, dispatched on `clientWideEncoding`, and
`TestIntegration_SqlplusLoginThroughSyntheticAuth` drives a real sqlplus login
through it end to end. That test runs in CI: when no sqlplus is on `PATH` it
uses the one bundled in the Oracle container the suite already starts, and
`ORACLE_TEST_REQUIRE_OCI_CLIENT=1` makes a run without any client a failure
rather than a skip — see "Where the OCI client comes from" above. The
byte-level evidence lives alongside it in `upstream_auth_client_wide_test.go`,
against a real capture.

The premise is measured, not assumed. With the wide dispatch disabled so a thin
body goes out on an OCI session, that sqlplus login dies with **ORA-03113**
(end-of-file on communication channel) and never authenticates.

Five things differ between the wide body and the thin one, all measured against
`testdata/sqlplus_cursor_reexec.pcapng` (macOS Instant Client 23.3 → Oracle 23ai
Free) and cross-checked against the DB-bundled 23.26 client:

| # | Delta | Thin | Wide (OCI) |
|---|-------|------|-----------|
| 1 | TNS data flags on the AUTH packet | `0x0000` | `0x2000` |
| 2 | Preamble encoding | compressed integers | pointer runs (`fe ff…`) + 4-byte little-endian fields |
| 3 | Key/value length fields | compressed | 4-byte LE; on the Instant Client a UTF-8 max-expansion **buffer** size (3x the CLR byte length), on the DB-bundled client a plain length |
| 4 | Empty value | CLR `0x00` | zero length field, **no CLR byte at all** |
| 5 | Logon mode | dbbat's own | carries client high bits (`0x40000000` captured in Phase 2) |

Conventions 3 and 4 cannot be derived from first principles, so
`detectWideAuthFraming` reads them off the client's own AUTH packet **for that
same phase** — Phase 2 carries different lead bytes and different logon-mode
bits from Phase 1 — falling back to the Instant Client shape. Only the framing
is borrowed; every value in the body is dbbat's own, the same division of labor
the rewrite path makes.

Which deltas the upstream actually *enforces* was measured the same way, by
breaking one at a time and re-running the login against Oracle 23ai Free:

- **Only the structure (2) is enforced.** It is what separates a readable body
  from ORA-03113.
- Oracle 23ai accepted the login with **thin data flags (1)**, and with **plain
  length fields in place of the 3x buffer sizes (3)** as long as every field
  agreed. The ORA-28041 trap is *mixing* the two conventions inside one body —
  the rewrite path's hazard, see `replaceAuthKVValueWide` — not picking the
  wrong flavor wholesale.
- **(4) is unreachable unless a value is genuinely empty**, which needs a host
  with no name; it is pinned byte-exactly by unit test instead.

All five are reproduced anyway: "this server tolerates it" is a weaker contract
than "this is what the client we are impersonating sends", and that tolerance is
unlikely to be uniform across the Oracle versions and OCI builds dbbat fronts.

Three keys the captured sqlplus sends are deliberately **not** reproduced —
`AUTH_CONNECT_STRING`, `AUTH_LOGICAL_SESSION_ID`, `AUTH_FAILOVER_ID`. All three
are informational, and the last two would have to be invented; the synthetic
body declares what dbbat is, not a fabricated OCI identity. For the same reason
`SESSION_CLIENT_CHARSET` and `SESSION_CLIENT_VERSION` keep the thin path's
values. What wide Phase 2 *does* add over thin is the four keys no thin client
sends (`AUTH_RTT`, `AUTH_CLNT_MEM`, `AUTH_ACL`, `AUTH_FLAGS`) and
`SESSION_CLIENT_LIB_TYPE=2` (OCI) rather than `0`.

Note also what the forced run does **not** exercise: with the rewrite disabled,
`rewriteAuthPhase1Username` / `parseAuthPhase2Header` never execute. Those are
the paths every real login takes, so a change to the preamble readers has to be
proved by the ordinary `make test-e2e-oracle`, and a change to the writers by
the forced one. Neither run substitutes for the other.

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
