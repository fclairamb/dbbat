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

SQL text is inside piggyback execute messages (func=0x03, sub=0x5e), and
**there is only one such op on the wire**. What dbbat used to call the "JDBC
exec" (`0x11`/`0x69`) is the close-cursors piggyback with an ordinary `03 5e`
execute stapled behind the close list; the "python exec" sub-op (`0x11`/`0x98`)
appears in **zero** frames across all 22 recordings in `testdata/`.

**The execute declares its own statement length — read it, do not search for
it** (`ttc_exec_statement.go`). Two header encodings carry it:

```
thin (go-ora, python-oracledb thin, JDBC thin, DBeaver):
  [0] 03  [1] 5e  [2] seq  [3] 00 (v315+ only)
  [..] options    TTC compressed int
  [..] cursorID   TTC compressed int
  [..] flag       1 byte, set when the cursor id is 0
  [..] sqlLen     TTC compressed int          <- the length
  [..] 01, 01 0d, the al8i4 array, zeros …
  [..] SQL text

OCI wide (sqlplus, SQL*Developer via OCI, Instant Client):
  [0..2]   03 5e seq
  [3]      01           constant
  [4]      seq+1        the NEXT message's sequence number
  [5..12]  options      8 bytes
  [13..20] fe x8        pointer sentinel
  [21..24] sqlLen * 3   uint32 LE — the client sizes the buffer for its widest
                        character encoding, and counts a trailing NUL when it
                        writes one
```

Verified byte-for-byte: go-ora's 56-byte `UPDATE dbbat_dml_test SET name =
'updated' WHERE id <= 3` carries `01 38`, DBeaver's 40-byte `ALTER SESSION SET
CURRENT_SCHEMA=TESTADM` carries `01 28`, and sqlplus's 23-byte `SELECT 1 AS n
FROM dual` carries `45 00 00 00`. Knowing the length reduces the search to "the
text run of exactly that length, bounded at **both** ends so it neither
continues a run that started earlier nor stops inside one that keeps going". The
far end matters as much as the near one: a length read too long cannot match,
but one read too short would return a silent prefix and the gate would enforce
against truncated text believing it was precise.

What counts as text is `shared.SanitizeStatementText`, the same judgement
`SanitizeQueryError` applies to diagnostics, and for the same reason: dbbat does
not know the session charset, so `INSERT INTO t VALUES ('café')` from a
WE8ISO8859P1 session is not valid UTF-8. A "valid UTF-8 or drop" rule sent such
a statement to the keyword scan, which truncated it at the accent — so a blocked
pattern or approval pattern in the tail stopped matching. Instead a run with
control bytes is refused (binary always has them, statements never do) and a run
with a few undecodable bytes is kept with those repaired to U+FFFD, which is
also what makes it storable in `queries.sql_text`. The header length, the
two-sided boundary and the leading verb are what discriminate a statement from
binary here, so this last test can afford to be charitable.

One consequence worth stating: for such a statement the text in `/queries` no
longer byte-matches the wire — the undecodable bytes are U+FFFD. That is a
deliberate trade, and the alternative was worse in both directions: a *truncated*
statement, and one Postgres would refuse to store. The HMAC chain hashes the
stored text, so verification stays self-consistent either way. Bind capture is
the one place the raw run is still needed, and it keeps it: `extractPiggybackBinds`
anchors its scan on the verbatim bytes, because a floor computed from the
repaired text would not match and a collapsed floor makes the scan read bind
values out of the statement's own text.

> **The scan this replaced was wrong on a quarter of real frames, and wrong in
> the direction that matters.** It looked for a length prefix at offsets 40–70
> (or 50–75) and then anywhere a SQL keyword appeared. But an ASCII byte
> *inside* a statement is a perfectly good length prefix — a space is 32, `T` is
> 84 — `looksLikeSQL` matched a keyword with no word boundary, and nothing
> required the run it named to be text at all. Measured over `testdata/`
> (`sql_extraction_survey_test.go`, run with `-v`): of 137 execute ops, the old
> scan agreed with the statement on 89 and returned a **mid-statement fragment**
> on 48. Three of those readings were enforcement failures rather than cosmetic
> ones:
>
> - `ALTER SESSION SET CURRENT_SCHEMA=…` read as `SET CURRENT_SCHEMA=…`. `ALTER`
>   is in both `writeKeywords` and `ddlKeywords`; `SET` is in neither — so
>   `read_only` and `block_ddl` did not fire on a statement they are written to
>   refuse, and `/queries` recorded the fragment. Nine of the corpus's execute
>   ops are this, five distinct statements, all of them DBeaver's connection
>   setup (`dbeaver.pcapng` 5, `dbeaver_init.pcapng` 4); no other client in
>   `testdata/` sends an `ALTER SESSION` **as an execute op**. The figure is
>   computed and asserted by `TestSurveyAlterSessionMisreadAsSet`, so
>   re-recording the corpus fails the test rather than silently ageing the prose.
>
>   Grepping the recordings for `ALTER SESSION` is misleading here and it caught
>   a reviewer: the string is in **all 22** files, as the `AUTH_ALTER_SESSION`
>   key/value inside the client's phase-2 AUTH message (dbbat emits the same
>   shape itself — `upstream_auth_client_wide.go`; the key is in the known set in
>   `ttc_auth.go`). AUTH is not a statement-carrying op: it is func `0x03` with
>   sub-ops `0x76` and `0x73`, the two rows the op table above lists as AUTH
>   Phase 1 and Phase 2. Those occurrences are an authentication attribute the
>   statement gate never sees — they were never gated and are not part of this
>   count.
> - go-ora's `UPDATE … SET name = …` read as `SET name = 'updated' WHERE id <=`.
>   Same bypass, on an ordinary DML statement with no adversary involved.
> - `SELECT 'YES' FROM USER_ROLE_PRIVS WHERE GRANTED_ROLE='DBA'` read as a
>   `GRANT` — `GRANT` matching the `GRANTED_ROLE` column name.
>
> This was filed as "a decoy `SELECT 1` ahead of the real `INSERT`". The
> measurement found it is not an attack shape at all: ordinary clients produce
> it constantly.

The old scan survives as a fallback for a header shape no recording produces,
narrowed on both axes: `looksLikeSQL` and the keyword scan now require the verb
to end at a **word boundary**, and a length-prefixed run must pass the text test
above (it previously had no such check at all). `findSQLInPayload`'s own byte
range is untouched — it still walks `0x0A..0x7E` inline rather than through the
text test — so widening what counts as statement text for the charset's sake
did not widen the last-resort scan by a single byte. `findSQLInPayload` also learned `TRUNCATE`, `GRANT` and `REVOKE` —
three verbs `writeKeywords`/`ddlKeywords` refuse but the scan could not see, so
a `TRUNCATE TABLE payroll` outside the window extracted as `""` and both paths
failed open. Adding verbs to a keyword scan normally *widens* the false-positive
surface, which on the unnameable path costs a session; it does not here because
the word-boundary rule removes strictly more than the three verbs add, and
`TestBundledOCIFixturesCarryNoStatement` pins that none of the recorded binary
frames reads as a statement.

### `ALTER SESSION SET …` and the statement gate

Once the gate saw `ALTER SESSION SET …` for what it is, `ALTER` being in both
`writeKeywords` and `ddlKeywords` meant a `read_only` or `block_ddl` grant
refused it — and DBeaver sends five of them *during connection setup*, so the
grant stopped the client from connecting at all rather than stopping it on a
write. (Under the pre-fix extractor the same statements reached the gate as a
bare `SET` fragment and slipped through; that was the bypass, not this.)

The resolution is a **parameter allowlist**, in `shared.IsAllowedAlterSession`
(`internal/proxy/shared/validation.go`), consulted by both `IsWriteQuery` and
`IsDDLQuery`: `ALTER SESSION SET <parameter>` is neither a write nor DDL when
**every** parameter it sets is on the list. It covers `CURRENT_SCHEMA`,
`OPTIMIZER_FEATURES_ENABLE`, the `_optimizer_*` hidden-parameter family, the
`NLS_*` formatting and collation settings and `TIME_ZONE` — session-scoped
settings Oracle reverts at disconnect.

It is a list of parameter names and **not** a prefix match on
`ALTER SESSION SET`, and the reason is `CONTAINER`: that one switches the
session's pluggable database, and a dbbat grant is scoped to one database, so a
prefix match would let a read-only session step outside what its grant covers.
`CONTAINER` is excluded; so is anything unenumerated, and a statement mixing an
allowed parameter with an unenumerated one is refused whole rather than
partially honoured. Anything the scanner cannot read end to end with confidence
— an unterminated quote, a missing `=`, a second statement stapled on behind a
`;` — is refused too.

Exclusion from the allowlist is not on its own enough for `CONTAINER`, because
all it buys is the pre-allowlist behaviour: the statement starts with `ALTER`,
so `read_only` and `block_ddl` refuse it and a grant with **neither** control —
the default, full write — allowed the switch. The reasoning that makes it too
dangerous for a read-only session applies unchanged to a writing one: a grant on
PDB1 is not a grant on PDB2, and every `queries` row written after the switch
names the wrong database. So `CONTAINER` also sits in `oracleBlockedPatterns`,
alongside `ALTER SYSTEM`, and is refused outright whatever the grant says
(`ErrOraclePatternBlocked`). The pattern scans to the end of the statement rather
than anchoring on `SET CONTAINER`, because `ALTER SESSION SET CURRENT_SCHEMA=X
CONTAINER=PDB2` performs the same switch with `CONTAINER` in second position.
`CURRENT_SCHEMA` stays allowed and is deliberately *not* caught: it moves
unqualified name resolution, Oracle still evaluates privileges as the connected
user, and a dbbat grant is scoped to a database rather than a schema.

An allowed `ALTER SESSION` is **still recorded** in `/queries` like any other
statement; this changes classification, not visibility. The five statements the
corpus carries are asserted against the allowlist itself by
`TestSurveyAlterSessionMisreadAsSet`, so a re-recording that introduces a new
parameter fails the suite instead of quietly reintroducing the connect-time
refusal. SQL Developer over the OCI driver is expected to send the same shapes;
that is inference, not measurement — it is not in the corpus.

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

**Those two frames are the whole gate.** There used to be a third reading, and
the intent behind it was sound — *a fetch arriving with no query in flight is a
re-execution, so gate it like a statement, while a fetch continuing a query
already in flight is left alone*. What it was wired to was not: the decoder
(`decodeOFETCH`) read a cursor id as a big-endian `uint16` at bytes 1..3 of a
message-type `0x11` frame, but `0x11` is the TTC **piggyback message type** and
bytes 1..3 are (function, sequence). A histogram of every client-side TTC op in
`testdata/*.pcapng` settles it: every `0x11` frame on the wire is `11/69`
(close-cursors) plus one `11/6b`, while every real fetch is message type `0x03`
function `0x05` (`TNS_FUNC_FETCH`) — 15 of them in `dbeaver.pcapng` alone. So no
Oracle client ever sent the frame the gate watched; the only frames that reached
it were piggybacks being misread, which is how the bundled OCI client's first
message became "a re-execution of cursor 27396" (see "Two OCI encodings, not
one"). The decoder, the handler and its tests are deleted.

Wiring the gate to the real `03/05` fetch remains available to whoever wants it:
the cursor id and fetch count are compressed ints after the header, and
`dbeaver.pcapng`, `python_thin.pcapng` and `sqlplus_cursor_reexec.pcapng` all
carry them. It was **not** done here on purpose. It is a behaviour change on the
hot path — the blast radius is "a fetch with nothing in flight", and refusing
there on a false positive breaks ordinary read-only work — so it needs the
false-positive rate measured against a live suite before it can be turned on.

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

The same scan reads the **fixed-width** OCI encoding of that OER, under the same
bounds plus that encoding's own RetCode anchor; which encoding is offered comes
from the session's learned `oerShape`, so a client known to speak compressed
integers is never scanned for a fixed-width block. Until that landed, cursor-id
learning was blind on every OCI session — see "The OCI client, on every shape"
below.

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
| `handleOERStatus`, **reporting a failure**, mid-row-stream | no, but the OER must **name the streaming cursor** | still possibly row bytes, so the diagnostic proof gets a second anchor — see "a failure raised mid-fetch" below |
| `handleOERStatus`, **reporting a failure**, outside a row stream | no | `decodeErrorOER` proves the tail is a diagnostic naming the code the fields reported — see the table above |
| `handleResponse`, mid-row-stream | **yes** | the payload *is* row bytes; a `0x04` run inside it is data |
| `handleResponse`, outside a row stream | no | the payload is a return-parameter block, and the anchors above are what stands in for the bit |

##### A failure raised mid-fetch

The second row used to read "**yes**", and its consequence was that a failure
raised **after rows had started flowing** was recorded with no error at all — the
statement stayed pending and the next one's `flushPendingQuery` closed it as a
success. That was accepted as a deliberate trade because no such failure had been
measured: every shape in the table above is raised before the first row, the
divide-by-zero included, because the server sends the OER *instead of* the
QueryResult.

It has been measured now, on **four** clients, and it does not hold.
`testdata/{python_thin,go_ora,jdbc_thin}_midfetch_fail.pcapng` and
`testdata/sqlplus_midfetch_fail.pcapng` (regenerate with
`go test -tags capture -run 'TestCapture_.*MidFetchFailure'`) record a
`TO_NUMBER` over a 20 000-row table whose row 15 000 will not convert, no
`ORDER BY` — a sort would materialize the set and raise before the first row,
which is the shape already covered — at array size 100. Every client fetches
**14 900 rows** and then takes an ORA-01722 that arrives the same way:

| | python-oracledb thin | go-ora | JDBC thin | sqlplus (OCI) |
|---|---|---|---|---|
| shape | standalone func `0x04` at byte 0 of a whole TNS Data packet | same | same | same |
| `CallStatus` | `0x1` | `0x1` | `0x1` | `0x1` |
| end-of-call bit | no | no | no | no |
| `CurRowNumber` | 14999 | 14999 | 14999 | 14999 |
| OER `cursorID` | 4 | 3 | 4 | 5 |
| the cursor dbbat was streaming | 4 | 3 | 4 | 5 |
| summary object encoding | TTC compressed | TTC compressed | TTC compressed | **fixed-width** |

So it is the same bit-less standalone shape as every other failure, only with the
column definitions and dozens of fetch round trips behind it — and `decodeOERAt`
dropped it exactly the way it dropped the other five.

###### The OCI client, on every shape

The last row is the finding the fourth client added, and for a while it was a
hole rather than a variant. sqlplus negotiates the **fixed-width** summary
object (the encoding `encodeOERFixedWidth` already *writes* for it — see "All
four drivers, five client builds, measured" below), so the seven leading fields
are little-endian integers at constant offsets rather than TTC compressed ones.
`decodeOERFieldsAt` reads compressed integers only and returned nil for the
whole block, so `decodeErrorOER` refused it, `handleOERStatus` never reached
either anchor, and the same blindness left cursor-id learning with no id for
that fetch to compare against.

The gap was **not** mid-fetch specific, which is why the anchor was never
touched for it: the same recording opens with a `DROP TABLE` of a missing table,
an ORA-00942 raised before the first row — the shape the rest of this section
says already works — and dbbat recorded that as a success too. On an OCI client
(sqlplus, Instant Client, SQL*Developer over OCI) *every* failing statement went
into `queries` as a success, and it was invisible: the client saw its ORA text,
the query row said nothing happened.

`decodeOERFieldsAtLayout` is the reading half of `encodeOERFixedWidth`, at the
very offsets that encoder writes. Read at `oerFixed32Layout`'s own offsets, the
recorded ORA-01722 carries call status `1` (no end-of-call bit) at 1, ECID 166
at 5, error number 1722 at 11 and again as the RetCode at 66, cursor id **5** at
17, call number 169 at 45, the row count 14999 compressed at 70, and the
`ORA-01722` CLR right behind it.
`TestDumpReplay_OCIMidFetchFailureIsFixedWidthAndReadable` asserts all of that
twice — as raw bytes and as what the decoder makes of them — and
`TestDumpReplay_OCIFailuresAreRecordedWithTheirORAText` covers the pre-first-row
ORA-00942 from the same recording. Live, `failed_stmt_integration_test.go` drives
four failing shapes through a real sqlplus
(`TestIntegration_FailingStatementsRecordTheirORAErrorOCI`).

Two things keep that decoder from being a new way to misread row data:

- **The RetCode anchor, not a length.** The error number at `errNum` must be
  repeated as the RetCode at `retCode`, which is the same invariant
  `oerFixedWidthTailFieldsAt` already validates the *encoder's* offsets with. It
  is the only structural proof available — every other field in the prefix is
  legitimately zero — so a run of zeroes is additionally rejected on a zero call
  status, and the callers keep the proof they already applied: the tail must
  still spell the code the fields report (`diagnosticNamesCode`), and the cursor
  bounds still apply to a status OER.
- **Which layout is asked, not sniffed.** The session learns `fixedWidth` /
  `fixedWidth64` from the upstream's own OERs, so a session known to speak
  compressed integers is offered no fixed-width reading at all. The ordering trap
  — the shape is learned from a *server* OER, so the first OER of a session can
  arrive before anything is known — is mostly closed by
  `readUpstreamAuthMessages`, which learns off the AUTH exchange before any
  statement runs; where it is not, the fallback is to try **both** layouts under
  the RetCode anchor rather than to accept the wrong one. `learnOERTail` also
  runs *before* `learnCursorID` on each upstream packet, so the OER that names
  the allotted cursor is itself allowed to teach the layout it is written in.

`testdata/sqlplus_midfetch_fail.pcapng` is therefore in `midFetchDumps()` like
the other three, which is what puts it through the cursor anchor and both
`RecordsItsORAText` tests instead of only a byte-level measurement.

What the mid-stream path accepts now is byte-0 routing **plus** `decodeErrorOER`
**plus** `midFetchOERNamesTheStreamingCursor`. The cost of the first two was
measured rather than argued, over the whole `testdata/` corpus replayed through a
real session so `rowStreamActive()` is the session's own
(`TestDumpReplay_MidStreamOERFalsePositiveRate`):

- across **26** recordings, **641** server packets arrive mid-row-stream;
- **4** of them begin with `0x04` — the four genuine ORA-01722s; 623 of the rest
  begin with `0x06`;
- `decodeErrorOER` accepts exactly those **4** and nothing else:
  **false-positive rate 0**. It was 3 before the fixed-width decoder, the fourth
  being sqlplus's, refused for being unreadable rather than for being row data;
- running the same predicate at *every* `0x04` offset **inside** all of those
  mid-stream packets accepts **0**. Nothing routes that way — `handleOERStatus`
  only ever sees byte 0 — so it is not a rate, it is how far real row data is
  from satisfying the proof.

Every one of those figures is printed by that test (`-v`), including the full
leading-byte histogram, so the numbers above are reproducible from the tree
rather than remembered. Only the two load-bearing ones are asserted — the
accepted count and the stress count — because pinning a distribution would turn
"somebody re-recorded a fixture" into a test failure.

The cursor anchor is not what that measurement justifies. It is aimed at the one
shape a corpus of numeric and temporal fixtures cannot contain: a result set
whose rows *carry* `ORA-` text, as `SELECT message FROM error_log` does. Such a
row would have to decode as seven bounded ints, be followed by the ASCII spelling
of the number its fourth field landed on, **and** have its seventh field land on
the streaming cursor's own id. It fails closed — a mid-fetch diagnostic naming
another cursor is dropped, and so is one arriving on a fetch whose cursor id was
never learned — precisely the old behaviour, with a DEBUG line saying so, so an
unmeasured client reporting a different cursor is visible rather than silent.

`handleResponse`'s mid-stream strictness, `findOERInResponse` and cursor-id
learning are untouched by this.

**Why there is no third anchor.** All three of the conditions above are, in
principle, influenceable by a *deliberate* client: row content through
`SELECT '<literal>'`, packet boundaries through the fetch size, and — on the
legacy `OALL8` path — the cursor id it names itself. Two further anchors were
considered and declined:

- `CurRowNumber >= pending.rowNumber`, monotonic against rows already captured.
  It is trivially satisfied whenever result capture is off, which is the default,
  so it would add a way to drop a real failure without adding protection in the
  common configuration.
- requiring the OER's decoded extent to account for the whole TNS packet. The
  OER tail has variable extra fields (`oerShape.extraTailFields`), so a tight
  bound is exactly the kind of anchor that goes stale on an unmeasured client.

The reason neither is worth its risk is that this is not the weakest link.
`handleResponse`'s mid-stream branch already calls `findOERInResponse`, which
scans **every** offset and accepts on the end-of-call bit alone, with no
diagnostic proof at all — a strictly cheaper forgery for the same effect. And the
payoff either way is bounded to falsifying the client's *own* query record. Each
additional anchor, by contrast, is a new way to silently drop a real failure on a
client nobody has captured, which is the bug this section exists to remove. There
is also one caveat worth stating plainly: `learnCursorID` latches only after it
succeeds, so on a statement whose id is never learned the anchored scan runs over
row-stream bytes for the whole fetch — the reference value this anchor compares
against could itself have come from row data. Pre-existing (re-execution gating
already trusts it), but now load-bearing for error text too.

Cursor-id learning is on none of those rows and its bounds were not touched: it
reads `findPlausibleOERInResponse`, which still refuses any OER reporting a real
failure, because such an OER assigns no cursor. It gained only the second
encoding — the fixed-width block above, under those same bounds plus the RetCode
anchor — which is what gives an OCI fetch a streaming cursor id for the anchor to
compare against at all. That same property is why it
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
`failed_stmt_integration_test.go` — which covers the mid-fetch case too
(`TestIntegration_MidFetchFailureRecordsItsORAError`) and the OCI client
(`TestIntegration_FailingStatementsRecordTheirORAErrorOCI`), so neither the
relaxation nor the fixed-width decoder is evidenced only by replayed bytes.

A re-execution naming a cursor dbbat cannot resolve goes through
`refuseUnknownCursor`, exactly like the SQL-less `OALL8`: refused under a grant
carrying a statement-shaped control, forwarded with a WARN under one carrying
none. See `docs/approvals.md`.

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

The same learned flag now also selects how dbbat **reads** an OER, not only how
it writes one (`decodeOERFieldsAtLayout`). That the two halves were once
asymmetric — writing an encoding it could not read — is why every failing
statement on an OCI client was recorded as a success; see "The OCI client, on
every shape".

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
| `gateStatement`, on the client leg | yes — the packet being refused is the call | the op just observed |
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

Both numbers are measured rather than reasoned, and the first one is *not*
generous — a large enough fetch batch crosses it. See "What a legitimate handoff
costs, measured" below.

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

###### All four drivers, five client builds, measured

`TestIntegration_AsyncRefusalAgainstOCIAndPythonThin` closes the two clients the
fix shipped without, on the same fixture and through the same recording tap.
Both risks that motivated it are ruled out: sqlplus did **not** hang, and no OCI
session took the unnameable fallback. Four drivers, but five rows below, because
the OCI client is two client builds with two different on-wire encodings and
both had to be run.

| Client | Rows drained first | What dbbat wrote | What the client reported |
|---|---|---|---|
| ojdbc 23.7.0.25.01 | 1500 of 5000 | one ORA-00028, call 8, compressed | `midfetch: code=28` |
| go-ora v3 | 1580 of 5000 | one ORA-00028, call 12, compressed | `driver: bad connection` — it maps ORA-00028 to a dead conn *by design* |
| python-oracledb thin 3.4 | 1002 of 5000 | one ORA-00028, call 7, compressed | `code=28`, rendered as `DPY-4011` |
| sqlplus, Instant Client 23.3 (macOS, on PATH) | 1000 of 5000 | one ORA-00028, call 17, **fixed-width 32-bit** | `ORA-00028: session terminated: bandwidth quota exceeded for this grant`, printed verbatim |
| sqlplus, bundled 23.26 (`gvenzl/oracle-free:23-slim`) | 1000 of 5000 | one ORA-00028, call 17, **fixed-width 64-bit** | the same ORA-00028, printed verbatim |

The two sqlplus rows are **two runs, not one**: `plannedOCIClient` is a
`sync.OnceValue` settled before any container starts, so the flavor is a
property of the process (`ORACLE_TEST_OCI_CLIENT=path|container` picks it). CI
has no client on PATH and therefore measures the bundled one only — the Instant
Client row has to be re-taken by hand after a change to the fixed-width
encoding.

Every one of the five took the **delivered** path — `logMsgRefusalHeld` then
`logMsgRefusalDelivered`, with `logMsgRefusalUnnameable` never once — and no
watchdog teardown and no overshoot abandonment anywhere. The sqlplus subtest
asserts both of those as itself rather than logging them: `delta.unnameable` is
pinned at zero, and the client's own stdout has to carry the ORA-00028 text,
which is what separates *reading the fixed-width frame* from *meeting a closed
socket* — everything else there is satisfied by a socket close too. So the call the client
makes after finishing a fetch reply is, on every client dbbat supports, one it
can name: the OCI piggyback that `gateUnnameableFrame` exists for is what these
clients open a *session* with, not what they resume a drained fetch with. The
statement after the refusal fails on every client (`ORA-03113` on sqlplus,
`DPY-1001` on python-oracledb), which is the session being over, as intended.

Two client-side details are worth keeping, because both make the error *text*
useless as evidence while the frame was read perfectly:

- **python-oracledb parses it and relabels it.** `code` is 28, but the exception
  renders as `DPY-4011: the database or network closed the connection` — the
  driver folds a killed session into its own connection-closed error, much as
  go-ora folds it into `driver.ErrBadConn`. Only the error *object* separates
  "parsed the ORA-00028" from "met a closed socket", which is why the probe
  reports `code`/`full_code` rather than the message.
- **sqlplus is the one client that prints the message unchanged**, and it is
  also the only one whose frame is in the fixed-width encoding — so it is the
  end-to-end evidence that `encodeOERFixedWidth` (both layouts) produces a frame
  an OCI client reads *when written from the client leg*, which is the moment
  this fix moved.

The tap needed one change to say any of this. A fixed-width summary object is
mostly zeroes, and a run of zeroes walks cleanly as compressed integers, so the
compressed decoder does not fail on an OCI frame — it *succeeds* and reports
ORA-00000 on call 0. `decodeTappedOER` now runs both decoders and arbitrates by
the frame's own "ORA-NNNNN" text, which is the one field neither encoding can
fake.

Confidence: the enumeration is read off the code and pinned by
`TestUpstreamToClient_ByteLimitHoldsRatherThanCuttingIn`,
`TestUpstreamToClient_StopsRelayingPastTheOvershootBound`,
`TestHeldRefusalEndsTheCallTheClientIsNextParkedOn`,
`TestHeldRefusalMeetingAnUnnameableCallClosesInstead`,
`TestHeldRefusalBlocksAFrameItCannotRead`,
`TestHeldRefusalIsAnsweredExactlyOnce`,
`TestHeldRefusalTearsDownEvenWhenTheFrameWriteBlowsUp`,
`TestHeldRefusalStandsTheWatchdogDownUntilItsGrace`,
`TestHeldRefusalFallsBackToTheCloseWhenTheClientStopsTalking`,
`TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut`,
`TestHeldRefusalStopsRelayingOnceTheOvershootBoundIsCrossed`,
`TestHeldRefusalRecordsWhatTheHandoffCost` and
`TestIdleLimitViolationSendsNoOER`; the live cases above are measured against
Oracle 23ai Free and pinned by the integration tests named with them. The driver
the ojdbc rows were taken on is **ojdbc 23.7.0.25.01**, not the 26.1 the
call-number finding is attributed to; what that does and does not license is in
"Which ojdbc these results are attributed to" above.

**Both fail-safes now fire without hand-mutated session state.**
`refusalHoldBytes` / `refusalHoldGrace` are per-session overrides of the two
constants (zero means "use the constant", so a session built without them keeps
the production bounds), and that is what makes the two paths reachable at all —
no live client produces a reply with no boundary, and no live client stops
talking on demand. `TestUpstreamToClient_StopsRelayingPastTheOvershootBound`
drives the real `upstreamToClient` over a `net.Pipe` against an upstream whose
reply never ends, and the **relay** is what runs out of bound: it returns
`ErrByteQuotaExceeded` having relayed within one packet of the bound.
`TestHeldRefusalWatchdogFallsBackAfterItsGraceRunsOut` arms the hold through
`enforceMidStreamLimits`, runs the real `LimitGuard.Watch` → `onLimitViolation`
with a millisecond grace and a client that never speaks, and asserts both
sockets dropped and the statement recorded `aborted: bandwidth quota exceeded
for this grant`. The two older tests stay: they pin the arithmetic and the
teardown, which is a different claim from "the relay and the watchdog get
there".

###### What a legitimate handoff costs, measured

Every exit of a held refusal now records the two quantities the bounds cap —
`relayed_bytes_since` (bytes relayed to the client past the violation) and
`held_for_ms` (how long the hold lasted) — and every suite that drives a live
client reads them back (`reportHandoffCost`). So the numbers below are
observations, not estimates.

The five client builds at the fetch size the suites use (500 rows of a 400-byte
payload, on loopback) are all microscopic against both bounds, exactly as the
reasoning predicted:

| Client | Fetch | Bytes past the violation | Held for |
|---|---|---|---|
| ojdbc 23.7.0.25.01 | `setFetchSize(500)` | 26 884 (0.3% of 8 MiB) | 0 ms |
| go-ora v3 | driver default | 19 | 3 ms |
| python-oracledb thin 3.4 | `arraysize=500` | 116 105 (1.4%) | 1 ms |
| sqlplus, Instant Client 23.3 (macOS, on PATH) | `SET ARRAYSIZE 500` | 127 497 (1.5%) | 10 ms |
| sqlplus, bundled 23.26 (`gvenzl/oracle-free:23-slim`) | `SET ARRAYSIZE 500` | 116 277 (1.4%) | 119 ms |

That is the *only* corner of the space those suites cover, and it is not the
worst case. Two variables move a handoff's cost and nothing else does: the
**fetch size**, which decides how much reply is in flight when the quota is
crossed, and the **link speed**, which decides how long those bytes take to
arrive — loopback divides the second to zero.
`TestIntegration_HeldRefusalHandoffCost` takes one client (python-oracledb thin)
to each extreme, the slow link through a rate-limited `recordingTap`:

| Case | Bytes past the violation | Held for |
|---|---|---|
| `arraysize`/`prefetchrows` 3000 on **4000-byte** rows (a ~12 MB fetch batch), loopback | **8 394 885 — the bound, crossed** | 88 ms |
| `arraysize` 2000 on 400-byte rows, over a **128 KiB/s** link | 537 122 (6.4%) | **6 541 ms (22% of the grace)** |

Both rows say something the loopback table cannot.

**8 MiB is not slack; it is roughly one large fetch batch.** At a 3000-row array
size on the widest non-LOB column Oracle has, the batch is about 12 MB, so a
successful handoff needed ~11.7 MiB — `refusalHoldMaxBytes` fired first, the
session ended on the socket close, and that client got `DPY-4011` with `code=0`
(a dead connection) instead of the ORA-00028 it would have got at a smaller
fetch size. Nor is there a larger value that would settle it: the array size is
the client's to choose, so no finite bound covers every legitimate handoff.

**The two bounds meet at a link speed.** 537 KB took 6.5s over the throttled tap
— an effective 80 KiB/s once the proxy and the tap are in the path — so a tail
at the full 8 MiB bound would take about 105s on that link, three and a half
times the grace. Below roughly 280 KiB/s of effective throughput the grace is
the binding constraint and the byte bound is unreachable; above it, the reverse.

**That crossing is accepted rather than designed away, and it is reported.** The
alternative was to make the grace conditional on progress — extend the wait while
bytes keep arriving, capped by `refusalHoldMaxBytes`, which turns the grace from
a deadline into an idle timeout and leaves a single binding bound. It was
declined: the grace is the fail-safe for *a client that stopped talking*, and an
idle timeout would let a trickling upstream hold an over-quota session for as
long as it takes to relay 8 MiB. dbbat normally sits next to the database, where
280 KiB/s is not a rate anything runs at, so the case is speculative — but it is
indistinguishable at the client from the other three fail-safes, all of which
surface as ORA-03113.

So the crossover names itself in the log instead. When the grace expires on a
hold that was **still being fed** — bytes moved past the violation, and the last
of them reached the client within a third of the grace — `onLimitViolation` emits
a second WARN alongside the teardown:

```
a held limit refusal was cut by its grace while the client was still draining the reply:
on this link the overshoot bound was unreachable
  relayed_bytes_since=… held_for_ms=… effective_bytes_per_second=… crossover_bytes_per_second=…
```

`crossover_bytes_per_second` is `refusalHoldMaxBytes / refusalHandoffGrace` — the
~280 KiB/s above — and `effective_bytes_per_second` is what this hold actually
drained at, so the line carries its own proof rather than an assertion. Nothing
branches on it and neither bound moves. A client that simply went quiet is not
reported this way: no bytes at all, or a tail that arrived early and stopped,
both fail the predicate (`heldRefusalWasStillReceiving`), which is the whole
point — the record is only worth anything if it separates a slow link from a
silent client. Its one blind spot is honest and bounded: a tail small enough to
fit entirely in the client socket's send buffer is handed to TCP in one go, so
the relay sees no progress while the client is still draining it. The crossover
only arises for overshoots far larger than a socket buffer.

**Both of those numbers — the window and the buffer — were first sized against
the wrong quantity, and the end-to-end subtest is what found it.** The record was
unit-tested from synthetic state and passed, but it never once fired against a
live slow client:

- the relay's writes are not paced by the packet rate. A write completes when
  the client socket frees space, and TCP frees it in receive-window updates,
  which arrive at roughly *half the receive buffer* at a time. Measured through a
  32 KiB/s tap, a relay writing continuously right up to the teardown still had
  **4.5s between its last two completed writes** — outside the tenth-of-the-grace
  window the predicate used, so the record was skipped. Hence a third.
- "a socket buffer" is not a few hundred kilobytes. A **683 100-byte** tail went
  into a loopback socket in **1.5s** and was still being drained by the client
  **100s** later, with nothing for the relay to see in between. On a real link
  TCP sizes that buffer to the bandwidth-delay product, so a poor link keeps it
  small; a userspace throttle on loopback does not. The subtest therefore has to
  drive megabytes to reach the regime a WAN reaches with hundreds of kilobytes.

`TestAbandonedHoldNamesTheCrossoverOnlyWhenTheClientWasStillDraining` pins the
three shapes as unit tests; the third subtest of
`TestIntegration_HeldRefusalHandoffCost` drives python-oracledb at a 1500-row
array size on 4000-byte rows (a ~6 MB tail) over a 32 KiB/s tap, where a real
tail outlasts the real 30s grace, and reads the record back — 1 589 756 bytes in
30 002 ms, an effective **52 988 B/s** against the 279 620 B/s crossover rate,
19% of the byte bound. When it declines instead, the same four numbers go out at
DEBUG (`a held limit refusal was abandoned at its grace, but not as a bound
crossover`), which is what makes a missing record diagnosable rather than
silent.

**The values are kept, and this is why.** Narrowing either would push clients
that are behaving normally onto the socket close, which is the pre-fix behaviour
the hold exists to replace — 128 KB and 6.5s are real observations, and a bound
near them would fire on ordinary traffic. Widening is the more tempting move
after the first row above, and it is still refused: both are *enforcement*
limits before they are ergonomics. 8 MiB is already a large overrun to allow
past an exhausted quota (28× the budget in the fixture that measures it), and
what a client loses beyond either bound is the **message**, not the enforcement
— the session ends either way, the statement is recorded with the real reason
either way, and the difference is ORA-03113 instead of ORA-00028.

They are also deliberately **not** operator-visible. The per-session override
fields (`refusalHoldBytes` / `refusalHoldGrace`) are the seam if that ever
changes, but a knob here would let a deployment buy a friendlier error code with
enforcement, and the only demonstrated symptom is that one cosmetic difference.
The measurement above is what an operator meeting it needs: a client seeing
ORA-03113 rather than ORA-00028 on a quota trip has a fetch batch over 8 MiB or
a link under ~280 KiB/s, and neither is a dbbat fault to fix by widening. Which
of the two it is no longer has to be inferred — the byte case logs
`ended the session: a held limit refusal never reached a call boundary`, and the
slow-link case logs the crossover record above.

##### The refusal that happens before the session: AUTH Phase 2

A client can be refused before there is a session at all — no active grant, a
service name matching several dbbat databases, a key that answers no verifier —
and `sendAuthFailed` writes that refusal. It used to write a frame of its own
invention: `0x00 0x00 0x08`, the ORA code as a compressed integer, and the
message as a bare CLR. That is not an error structure, and **python-oracledb thin
turned every AUTH-phase refusal into a driver bug**:

```
DPY-5002: internal error: read integer of length 39 when expecting integer
of no more than length 4
```

39 is not a protocol quantity. It is `len("invalid username/password; logon
denied")`. The driver read type `0x08` as a return-parameters message, called
`read_str_with_length` → `read_ub4`, landed on the CLR's length byte and reported
it as an integer width. The three refusals therefore reported 39, 59 and 92 — the
lengths of dbbat's own three messages — and the reporter concluded the thin
driver was at fault and began porting to thick mode, which would not have helped.

The frame is now the same OER `writeTTCError` sends, from the same builder
(`encodeOERPacket`); `buildAuthFailed` is gone. Two fields the mid-session path
gets for free have to be supplied here:

**The call number.** `observeClientCallNumber` runs on the proxy loop, which a
session refused at AUTH never reaches, so the refusal would end call zero — the
ORA-18745 case above. `observeAuthCallNumber` takes it off the newest AUTH packet
instead: Phase 1 for a client refused before the challenge, Phase 2 for one
refused on its key. Both are ordinary TTC ops (`03 76 <seq>` / `03 73 <seq>`).

**The summary's trailing field count**, which is the one that cannot be left at
its default. Mid-session an unlearned tail costs the message text and nothing
else, which is why `nextOERFrame` writes zero until an upstream OER teaches it
better. At AUTH the text *is* the fix — "request access via dbbat" is the whole
point of the frame — and a client that expects the two 20c fields and does not
find them lands on the message's length byte, which is DPY-5002 again, this time
inside a well-formed OER. `authRefusalOERShape` therefore guesses, and the guess
is **customHash**: a modern client (the 12c/18453 challenge) gets two, a legacy
one (go-ora's 6949) gets none. That is the line the two hard-coded challenge
summaries in `buildAuthChallengeEndMarker` already fall on — decode them and the
6949 one carries no extra fields, the 18453 one carries two, and the 153-byte OCI
capture carries two in the 64-bit fixed-width layout — and it is the line the
real server OERs in `ttc_oer_encode_test.go` fall on. It is **not** the negotiated
TTC field version: go-ora's is the *higher* of the two (see "The AUTH function
header is negotiated, not fixed"), so a version rule gets both clients backwards.
A tail learned off a real upstream OER still wins, and an OCI session has one
before it is ever challenged (`beginUpstreamAuth` runs first).

`TestIntegration_AuthRefusalAcrossClients` revokes the fixture user's grant and
meets the refusal with each client. Measured against Oracle 23ai Free:

| Client | What the client reported |
|---|---|
| python-oracledb thin 3.4.2 | `class=DatabaseError code=1045 full_code=ORA-01045`, message `ORA-01045: no active grant for this database; request access via dbbat` — where it used to report DPY-5002 |
| sqlplus, Instant Client 23.3 (macOS, on PATH) | `ORA-01045: no active grant for this database; request access via dbbat`, printed verbatim |
| go-ora v3 | `ORA-1045 error occur at position: 0` — the code, its own wording |

A fourth subtest takes the *post-challenge* refusal, which is where the summary
tail follows the verifier the challenge was actually built with rather than the
negotiation: with a grant in place and a bogus key, python-oracledb reports
`class=DatabaseError code=1017 full_code=ORA-01017`, message `ORA-01017: invalid
username/password; logon denied` — the 39-byte message whose length DPY-5002 used
to report. It runs last because it is the one case that needs a grant: a user
without one is turned away before their key is looked at, so every other subtest
reports ORA-01045 whatever password it sends.

The sqlplus row is the flavor `plannedOCIClient` settled on for that run (a
client on PATH wins when there is one); CI has none and measures the bundled
23.26 client instead, which takes the 64-bit fixed-width layout — the same split
the mid-reply table above spells out.

The go-ora row is the accepted cost of the guess, and it is a real trade rather
than an oversight. go-ora negotiates customHash like a modern client but parses
like a legacy one — its summary reader takes the message CLR straight after the
wide RetCode pair — so it reads dbbat's first trailing 20c field as an empty
message and renders the code itself. The two layouts are mutually exclusive: no
byte sequence puts a non-empty CLR where go-ora looks *and* two integers where
python-oracledb looks. Given that, the tail goes to the client that **cannot
parse the frame at all** without it over the one that loses a sentence. The
refusal's ORA code reaches both.

The probe reads python-oracledb's exception *object* — `class`, `code`,
`full_code` — and not its text, for the same reason the mid-fetch probe does: the
failure being measured produced an `InternalError` with no ORA code anywhere in
it, so "the driver parsed the refusal" and "the driver could not read the frame"
are indistinguishable from the message alone.

**The third refusal — an ambiguous service name — is covered by unit test only,
and that boundary is deliberate.** It is the one the original report was about
(`MUTU01`, a service name shared by several dbbat databases), and it is issued
from a different branch: `disambiguateDatabase`, which runs *before* the
challenge, off the username in AUTH Phase 1.
`TestDisambiguateDatabase_RefusesWithAReadableORAError` drives that branch against
a real store — three server rows sharing one `oracle_service_name`, grants on
two of them — and follows the same three steps `run()` does
(`disambiguateDatabase` → `authRejectFor` → `sendAuthFailed`), decoding the frame
off the socket: the ORA code, the whole 92-character sentence, the OER's own
field walk, and the call number of the Phase 1 the client is parked on. Its
sibling subtests cover the other two outcomes of the same branch (no grant on any
candidate → ORA-01045 "no active grant", exactly one → that database selected).

What no test does is drive it with a *live client*, and the reason is
`resolveDatabase`: it resolves the connect string against a dbbat server **name**
first and only falls through to the shared-service-name candidate list when that
misses. The Oracle fixture's single server is named after the service it serves,
so reaching the candidate list from `TestIntegration_AuthRefusalAcrossClients`
means arranging for that name *not* to match the connect string mid-fixture.
That is now possible without deleting the row: `store.ServerUpdate.Name` renames
a server (`PUT /api/v1/servers/:uid`), so a fixture can rename its row to
something the client never sends and keep the grants and history hanging off its
`database_id`. A live-client version of this case is therefore feasible for the
first time — it is simply not written yet. The frame is the same one the two
measured refusals carry (same builder, same shape decision, same summary tail),
so what goes unmeasured is the *branch's* wiring, not the encoding — and that is
what the unit test pins.

##### Keys that cannot do Oracle, and the refusal that says so

O5LOGON needs a verifier derived from the API key. Keys are Argon2id-hashed, so
that verifier cannot be recovered afterwards: it exists only if it was derived at
mint time. **Every key minted before Oracle support therefore authenticates
against the REST API and every other protocol and can never be used for Oracle**
— and until 0.25 nothing said so anywhere. The production sighting (2026-08-13, a
key created 2026-07-12) is the shape of it:

```
O5LOGON verifiers loaded — any of these API keys works for Oracle login
  candidates=3 primary_key_prefix=dbb_8kre has_18453=true
AUTH Phase 2: candidate did not decrypt AUTH_PASSWORD  key_prefix=dbb_8kre …
WARN client authentication failed  error="API key verification failed: no candidate key decrypted AUTH_PASSWORD (3 tried)"
```

The three candidates are the user's *other*, newer keys. The key actually being
presented is not in the list at all — that is what makes it invisible — and the
client saw `ORA-01017 invalid username/password`, indistinguishable from a typo.
Minting a new key happens to fix it, which hides the cause and teaches nothing.

**The listing.** `GET /api/v1/keys` (and the single-key fetch) now carry
`oracle_capable`, shown in the UI's key list as a per-row badge plus a banner
counting the live keys that cannot do Oracle. The predicate is a *decryption*,
not a column test — `store.APIKey.DecryptedO5LogonVerifier6949`, which is the
same call `decryptVerifierData` makes to build a challenge, so the listing and
the proxy cannot disagree. Material that no longer decrypts under the running
`DBB_KEY` is reported unusable for exactly the reason the proxy treats it as no
candidate. The field is a `*bool`: absent when the process has no master key and
therefore cannot tell, so "unknown" never reads as "unusable".

**The refusal.** dbbat cannot say *which* key was presented. It challenges from
the user's shared salts and learns only that no candidate decrypted
`AUTH_PASSWORD`; the presented key left no trace, and in this failure it was
never a candidate. What it can say — and now does — is the strictly weaker,
still-actionable thing: when a login is refused and the user owns live keys with
no usable verifier, the message counts them and says to mint a new one. The code
stays **ORA-01017** (the credential really was refused) and "invalid
username/password" stays the primary clause, because dbbat does not know the key
just typed was one of them. It is one more case in `authRejectFor`, carried there
by a typed error (`keysWithoutVerifierError`) so the sentinel callers classify by
survives, and it rides the same OER frame as the refusals above. Counts and
advice only: no prefixes, no names, nothing derived from the material — the
precedent for naming an account-shaped fact to an unauthenticated client is
`ErrNoActiveGrant`, on this same frame.

**Backfill-on-use was considered and rejected.** A key presented over REST
arrives in plaintext, so its verifier *could* be derived and written at that
moment, silently healing every legacy key on first use. It is not implemented, and
should not be: verifier material is encrypted rather than hashed — unlike the key
hash itself — so writing it for every key that has ever been used widens what a
stolen store yields, to save the user one key rotation. The store's
"a leaked database yields no usable key" property is worth more than the
convenience. Reporting the fact is the honest fix.

##### Refusals on Oracle 19c: what is covered, and what is still outstanding

Everything above is measured against **Oracle 23ai Free**, because that is the
only image with no licence and therefore the only one an integration suite can
start. 19c is the version most dbbat deployments actually proxy, and it is not
interchangeable: the refusal frame is shaped from what the session negotiated,
and 19c negotiates differently.

**The production measurement that opened this.** On 2026-08-13, against
`dbbat.tools.stonal.io` running image **0.23.2**, upstream `Oracle Database 19c
Standard Edition 2`, under a `read_only` grant, with python-oracledb 3.4.2 thin:

- dbbat blocked the write correctly — `WARN query blocked by access control …
  write operations not permitted with read-only access`.
- the client never received it. A first run **hung past two minutes** with no
  response. With `conn.call_timeout` set, the call came back after 30s as
  `DPY-4011: the database or network closed the connection`, the connection was
  dead for every subsequent statement (`DPY-1001`), and the interpreter exited on
  a segfault (139) during driver teardown.

That is the exact opposite of what `TestIntegration_BlockedStatementRefusesPythonThin`
asserts and passes on 23ai. **0.23.2 predates the whole mid-session refusal
rework** — `ttc_oer_encode.go`, "end the client's call on a refusal with a real
OER frame", "refuse sqlplus/OCI in the encoding and framing it waits for" — so
it is a measurement of code that no longer exists. Whether current code still
fails that way on a live 19c is **not established here**; that re-measurement
needs a deploy and a 19c instance, and it is outstanding.

**What is established, and runs in CI on every commit:**
`internal/proxy/oracle/refusal_19c_test.go`. The corpus already contains real 19c
recordings — the premise that it was 23ai-only is wrong. `go_ora.pcapng` and
`dbeaver_init.pcapng` carry `Oracle Database 19c Standard Edition 2 Release
19.0.0.0.0 - Production` verbatim; `python_thin.pcapng` and `dbeaver.pcapng`
carry the same server fingerprint (42-entry capability array, `caps[7]` = 12)
and none of the 23ai captures do (54 entries, `caps[7]` = 27).
`TestOracle19cCaptures_AreReallyA19cUpstream` pins that provenance, because it is
what licenses the word 19c in the rest of the file.

`TestRefusalOn19c_ReachesTheClientOnAStillUsableSession` then replays one of
those recordings through the *production* observers — `observeCustomHashFlag`,
`observeBigClrChunksFlag`, `observeOERServerCaps`, `observeClientAuthEncoding`,
`observeOERClientVersion`, and `interceptUpstreamMessage`, so the OER layout is
learned off that 19c server's own end-of-call frames — arms a restrictive grant,
and drives blocked statements through the real `clientToUpstream` relay over a
pipe pair. It asserts all three properties the measurement above lost, not just
the error text:

| property | how |
|---|---|
| an ORA error arrives | the frame is decoded by the same strict walk the AUTH refusal uses (`decodeAuthRejectOER`), so the message CLR has to sit exactly where *this session's* negotiated shape says — a tolerant "scan for ORA-" decode passes on a frame no client can parse |
| it arrives promptly | every read carries a 5s deadline. The failure being guarded against is a two-minute hang, so a reintroduced one fails the suite in seconds instead of hanging it |
| the connection survives | the next statement is read off the *upstream* socket unchanged, a second refusal is answered on the same session with an advanced sequence number, and the relay ends on the client's own EOF with no error |

Covered refusals: `read_only`, `block_ddl`, the Oracle pattern list (`ALTER
SYSTEM`, which is the one refusal a full-write grant still meets), and an
exhausted quota. `block_copy` is covered by *asserting it refuses nothing* — an
Oracle COPY is a client-side SQL\*Plus command, never a server statement, so it
is deliberately absent from `hasStatementControls`, and a write under a
block_copy-only grant is forwarded.

**The statement being refused is synthetic, and cannot be otherwise.** No
recording contains a refused statement — nobody records a session being refused —
and the refusal frame is dbbat's own invention rather than something a server
sends, so there is nothing to replay for it. The exec frames come out of this
package's own builders in the wire shape each recorded client used (the piggyback
exec for go-ora, the `11 69` close-list-plus-execute for DBeaver/JDBC thin). The
quota subtest is the one that refuses a **recorded** 19c exec frame byte for
byte, since a quota refuses every statement rather than only a write.

Two things fell out of the replay and are pinned as their own tests, because both
bear on the frame a 19c client can read:

- **19c sends no trailing summary fields, to any client.** The two are Oracle's
  "fields added in Oracle Database 20c", and 19c predates 20c. The same
  python-oracledb thin build that gets **two** from 23ai gets **none** from 19c
  (`TestOracle19cSummaryTail_IsWhatThatServerSends`). dbbat is right by
  construction here rather than by luck: mid-session `nextOERFrame` leaves the
  count at zero until an upstream OER teaches it, and on 19c what it learns is
  zero. A change that started *assuming* the 20c tail would break the version
  that has no such fields.
- **On 19c the tail is learned before the AUTH-phase refusal can need it.** The
  upstream's AUTH **Phase 1** response already carries a summary object, so
  `authRefusalOERShape`'s modern-client guess (which would be wrong on 19c) never
  applies — the learned shape wins one packet before a Phase 2 refusal
  (`TestOracle19cLearnsItsTailBeforeAuthPhase2`).

**Not covered:** a live 19c client end to end. There is no licence-free 19c image,
and a suite leg that can never run in CI is not coverage, so no opt-in
licensed-image build tag was added. What the tests above measure is the frame and
the session, against a real 19c negotiation; what only a live 19c can settle is
the re-measurement of the production symptom, which stays with the owner.

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

#### The upstream comparison is textual, and the conflict is reported instead

The "all candidates must share the same upstream `host:port`" check compares the
addresses **as strings**. A CNAME in one row and the A-record of the same machine
in another therefore read as two upstreams, and the connect is refused ORA-12514
even though every candidate points at one database. That happened in production:
three rows carried `oracle_service_name = MUTU02` under
`oracle-abymutualise02.db.stonal.io` and
`abymutualise02.cusruf0cguz3.eu-west-3.rds.amazonaws.com`.

The compare stays textual on purpose. Resolving each candidate's host would put
a DNS lookup on the connect path and could answer differently between two
connects of the same DSN, turning a deterministic refusal into an intermittent
one. So the *misconfiguration* is surfaced rather than worked around:

- `store.OracleServiceNameConflictFor` is the single definition of "these rows
  disagree", shared by the proxy and by the two fleet queries
  (`ListOracleServiceNameConflicts`, `GetOracleServiceNameConflict`) — a second,
  SQL-shaped implementation would be exactly the drift that lets the UI call a
  configuration healthy while the proxy refuses it;
- the admin server listing, the per-row `GET /api/v1/databases/{uid}` and the
  create response carry `oracle_service_name_conflict`, and the servers page
  renders an amber marker on the service name naming every row and address
  involved;
- the connectivity check reports it as an advisory `warnings` entry
  (`oracle_service_name_conflict`) — the check itself still passes, because the
  row *is* reachable; what is unreachable is the shared service name;
- the refusal names the service name, the number of claiming rows and the number
  of upstreams, and logs the full conflict (row names included) at WARN. The row
  names deliberately stay out of the wire message: `resolveDatabase` runs before
  authentication, and the same reasoning that keeps ORA-01017 generic applies.

Soft-deleted rows and non-Oracle rows never raise a conflict, and only the
dedicated `oracle_service_name` column counts — never the database-name fallback
`oracleServiceName` uses for probes — because that column is what the candidate
lookup keys off.

#### The full connect descriptor as an escape hatch: spaces yes, parentheses no

When a dbbat server name is not EZ-Connect-safe, the connection endpoint falls
back to the raw upstream service name, which is what lands a client in the
shared-candidate path above. A client *can* sidestep EZ-Connect by sending a full
connect descriptor, but only so far — measured in
`TestParseServiceName_NamesEZConnectCannotExpress`:

| Client sends | `parseConnectDescriptor` yields | Usable? |
|---|---|---|
| `(CONNECT_DATA=(SERVICE_NAME=abyla mutu ro))` | `abyla mutu ro` | **yes** — spaces round-trip |
| `(CONNECT_DATA=(SERVICE_NAME="abyla mutu ro"))` | `"abyla mutu ro"` | no — the quotes stay in the value |
| `(CONNECT_DATA=(SERVICE_NAME=abyla_x (R/O)))` | `abyla_x (R/O` | no — truncated at the first `)` |
| `(CONNECT_DATA=(SERVICE_NAME="abyla_x (R/O)"))` | `"abyla_x (R/O` | no — quoting does not rescue it |

So the descriptor is a genuine escape hatch for a name containing **spaces**, and
nothing else: quotes are not syntax to this parser, and `serviceNameRe`'s value
class (`[^)]+`) cannot carry a parenthesis at all. Teaching it to would also mean
teaching `rewriteServiceName` the same quoting — it locates the value in the live
packet from the parsed text — so it is not a parser-only change. The durable fix
is the naming rule: server names are slugs
(`specs/todos/2026-08-13-23-server-names-must-be-slugs.md`).

## Known Limitations

- **Any API key works for Oracle login (per-user salts)**: The Oracle username from TTC AUTH Phase 1 maps to the dbbat user (lowercased) for grant checks and connection tracking, and any of that user's API keys created since the per-user-salt scheme can authenticate — see "Per-user O5LOGON salts" below. Two caveats: keys created before the scheme (legacy per-key salts) still fall back to first-key-only behavior until a new key is created, and clients that send an empty `AUTH_PASSWORD` (SQLcl / JDBC thin 23c+) cannot be disambiguated — dbbat assumes the most-recently-created user-salt key.
- **Fetches are not gated**: dbbat intercepts no fetch op. It used to carry a `0x11` fetch reading that gated "a fetch starting a fresh pending query" as a re-execution, but message type `0x11` is the piggyback message type and no client sends a fetch that way — real fetches are `03/05`, which dbbat does not intercept — so the reading was only ever reached by misparsing piggybacks (the bug under "Two OCI encodings, not one"). It has been deleted; the two re-execution frames that are real (the SQL-less `OALL8` and the `03/0x4e|0x04` piggyback) are enforced unchanged. Wiring the gate to `03/05` is a behaviour change on the hot path and needs its false-positive rate measured on a live suite first — the reasoning is kept under "Cursor re-execution".
- **Row capture is best-effort**: The TTC binary format varies across Oracle client versions. Some clients/query types may produce partial or no row capture. SQL text extraction works reliably across all tested clients.
- **Column names**: Real column names come from the describe column-definition records (`parseColumnDescribes` in `describe.go`), so single-char aliases (`SELECT level AS n`) and unnamed expressions (`SELECT count(*)`) get their true names and positions. Only genuinely unnamed expression columns fall back to a synthetic `COLn` label. If the records don't parse on some server layout, decoding falls back to heuristic name-scanning plus describe-header count padding, so the column count (and row framing) stays correct.
- **DML row counts**: INSERT/UPDATE/DELETE affected-row counts are captured from the v315+ OER status block (TTC func `0x04`, embedded in the execute Response) and stored as `rows_affected`, for clients whose OERs carry the end-of-call bit and (since the fix above) for those whose don't. **Failed statements record their ORA error text on every client**, out of the *standalone* func `0x04` that is how failures actually arrive — see the measurement under "the OER end-of-call bit is not universal", which found the bit to be a property of the call rather than of the client. That now includes a failure raised **mid-fetch**, once column definitions are decoded — measured at 14 900 rows into a 20 000-row fetch on **four** clients, and accepted there only when the OER also names the cursor whose rows are streaming; see "A failure raised mid-fetch". "Every client" includes the OCI ones (sqlplus, Instant Client, SQL*Developer over OCI) only since `decodeOERFieldsAtLayout`: they marshal the summary object fixed-width, dbbat read TTC compressed integers only, and until then *every* failing statement on those clients — mid-fetch or not — was recorded as a success. What is still not covered is a mid-fetch failure whose OER names a *different* cursor (none has been observed; it fails closed to the old no-error behaviour and logs a DEBUG line), and any mid-fetch failure on a client not captured. See `ttc_oer.go`.
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

**19c is not covered end to end, and cannot be:** there is no licence-free 19c
image to start a container from. What *is* covered on 19c, off the recorded 19c
sessions in `testdata/`, is the mid-session refusal frame and the negotiation it
is shaped from — see "Refusals on Oracle 19c: what is covered, and what is still
outstanding" above, which also records the one production symptom still awaiting
re-measurement.

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

The two columns are two *recorded servers*, not two guesses: the 42-entry array
is `go_ora.pcapng`, `dbeaver_init.pcapng`, `dbeaver.pcapng` and
`python_thin.pcapng` — the first two carry the 19c banner verbatim — and the
54-entry one is every other capture in the corpus (Oracle 23.26). Which capture
is which version is pinned by `TestOracle19cCaptures_AreReallyA19cUpstream`; the
19c ones are what "Refusals on Oracle 19c" above replays.

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
`decodeOFETCH` read a big-endian cursor id out of bytes 1..3, so on the bundled client's
very first message — `11 6b 04 …` — it read (function, sequence) and produced cursor id
`0x6b04` = 27396, which no session had ever opened. Under any statement-shaped control that
is a refusal, on the first call of every session. The Instant Client sends the same frame
and was refused identically; it just shrugs the refusal off, which is why this went
unnoticed for as long as the PATH flavor was the only one running. (That decoder and the
re-execution gate behind it have since been deleted outright — no client sent the frame
they watched. See "Cursor re-execution".)

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
live exec.

The carve-out survived the measurement that was meant to settle it, and it is worth being
precise about what did and did not change. `decodeExecSQL` reads the execute's declared
length now (see "SQL Extraction"), so the ordinary case — a `11 69` close list that **walks**,
with `03 5e <exec>` behind it — is decoded from the stapled op's own header and never scans
loose bytes.

**The exposure this carve-out names is unchanged, though.** When the close list does not
walk, the offset-0 anchor calls `decodeExecSQL` on the whole payload; the precise decode
declines (a `11 69` header is not an exec header, and `closeCursorsEnd` has already failed),
so the window scan and `findSQLInPayload` run over the whole frame exactly as before. A
`03 5e` elsewhere in the payload adds a *second* anchor that decodes precisely — it does not
suppress the offset-0 loose scan. A caller-supplied module string that reads as a refused
statement therefore still ends the session, which is the trade this carve-out was always
making.

**Every anchor is gated, not the first that answers.** A frame that staples two executes
runs both, so enforcing against one of them would leave the other exactly the smuggling
channel this path exists to close (`stapledStatements`). Duplicates are dropped, because the
recorded `11 69 <closes> 03 5e <exec>` shape offers two anchors for the one statement it
runs.

Three things the audit of this path filed for measurement, now measured over every recording
in `testdata/` (`sql_extraction_survey_test.go`):

- **Frames that are both unnameable *and* a cursor re-execution: zero.** 347 client frames,
  1 unnameable, 12 cursor re-executions, no overlap. An unnameable re-execution carries no
  statement text, so closing that hole would mean tearing the session down on a shape dbbat
  cannot identify — a false positive there costs a live session for a dbbat parsing bug
  rather than a user policy violation. It is left **forwarded** and the item is closed as
  unreachable in practice; `TestSurveyUnnameableReexecution` fails if a re-recorded corpus
  ever produces one.
- **A decodable legacy `0e` OALL8 stapled behind a `0x11` piggyback: zero.** Six `0x0e`
  bytes appear at a non-zero offset inside a piggyback and none of them decodes as an OALL8
  carrying plausible SQL, so `0e` stays out of `ttcStatementOpHeaders`: adding it would buy
  nothing and cost the false positives anchoring removed. Whether Oracle would *execute* such
  a frame remains unmeasured — no recorded client sends OALL8 at all — and the list does not
  depend on the answer.
- **The extractor's own weakness**, which was the real one: it returned the first hit rather
  than the executable one and its keyword list omitted `TRUNCATE`/`GRANT`/`REVOKE`. Both are
  fixed under "SQL Extraction" above, and the fix *narrowed* what the loose scan accepts
  rather than widening it.

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
