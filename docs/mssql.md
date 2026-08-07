# SQL Server Proxy — Protocol Notes

DBBat is growing a proxy for Microsoft SQL Server, so that queries against SQL
Server get the same observability, grants, quotas and approval holds as the
PostgreSQL, Oracle, MySQL and MongoDB targets.

The listener defaults to `:1434` (`DBB_LISTEN_MSSQL`; empty disables it). The
`+1` convention the other proxies use — upstream port plus one — works here
because the service that owns 1434 upstream, the SQL Server Browser, is
**UDP-only**; 1434/tcp is free.

> **Status: complete (stages 1–3).** SQL Server is a first-class protocol: a
> client authenticates against dbbat's own users and API keys, dbbat opens its
> own session on the target with the stored credentials, every statement is
> logged and checked against the grant, approval holds fire, and result rows
> and byte counts are accounted for — the same pipeline the PostgreSQL, Oracle,
> MySQL and MongoDB proxies run. The known gaps are listed under
> [What dbbat does not see](#what-dbbat-does-not-see) and
> [What is deliberately unsupported in v1](#what-is-deliberately-unsupported-in-v1).

## Library Choice

No Go library offers a TDS *server*, so the server side is hand-rolled, like the
Oracle and MongoDB proxies.

The **client** side was a real decision, not an assumption.
`github.com/microsoft/go-mssqldb` was read first: every wire-level type in it is
unexported — `tdsSession`, `tdsBuffer`, `writePrelogin`, `readPrelogin`,
`sendLogin`, `connect`, `getTLSConn` — and the only exported surface is the
`database/sql` driver and its `Connector`, which hands back a `driver.Conn` with
no access to the socket or the token stream. A proxy needs a raw authenticated
connection it can relay packets over, so the client half is hand-rolled on the
same codec as the server half (`internal/proxy/mssql/upstream.go`).

`go-mssqldb` therefore stays a **test** dependency only, driving the integration
suite as a genuine third-party client.

## Framing

`internal/proxy/mssql/packet.go`.

Every TDS message is a sequence of packets. Each packet is an 8-byte header
followed by a payload:

| Offset | Size | Field | Notes |
|--------|------|-------|-------|
| 0 | 1 | Type | `0x12` PRELOGIN, `0x10` LOGIN7, `0x01` SQLBatch, `0x03` RPC, `0x04` tabular result, … |
| 1 | 1 | Status | bit `0x01` EOM, `0x02` IGNORE, `0x08` RESETCONNECTION |
| 2 | 2 | Length | header **and** payload, big-endian |
| 4 | 2 | SPID | big-endian |
| 6 | 1 | PacketID | restarts at 1 per message, wraps at 256 |
| 7 | 1 | Window | always 0 |

Two things catch people out:

- **The header is big-endian, everything after it is little-endian.** The
  payload of LOGIN7, the token stream, the lot — all little-endian. Only the
  8-byte header is in network order.
- **A logical message spans packets until one has the EOM bit set.** The codec
  reassembles them, checks that every packet of a message agrees on its type,
  and caps the reassembled size (16 MiB) so a peer that never sets EOM cannot
  make the proxy allocate without bound.

The packet size starts at 4096 and is what LOGIN7 asks for thereafter, clamped
to [512, 32767]. PRELOGIN and the TLS handshake always run at 4096.

## PRELOGIN and the encapsulated TLS handshake

`internal/proxy/mssql/prelogin.go`, `internal/proxy/mssql/tlsconn.go`.

PRELOGIN (`0x12`) carries an option table — 5-byte entries of
`token / offset(BE16) / length(BE16)`, terminated by `0xFF`, followed by the
blobs those entries point at. dbbat parses VERSION, ENCRYPTION, INSTOPT,
THREADID, MARS, TRACEID, FEDAUTHREQUIRED and NONCEOPT, and answers with
VERSION, ENCRYPTION, INSTOPT (`0x00` = instance accepted), an empty THREADID
and MARS (`0x00` = refused). FEDAUTHREQUIRED and TRACEID are echoed only when
the client offered them — an unsolicited option upsets some drivers.

**The PRELOGIN response goes out as a Tabular Result (`0x04`) packet, not a
PRELOGIN (`0x12`) one.** The option-list *payload* is the same shape, but the
packet type is not symmetric, and clients enforce it: `go-mssqldb` rejects
anything else with `invalid respones, expected packet type 4`. The TLS
handshake packets that follow revert to `0x12` in both directions. This was
found by pointing a real driver at the proxy, which is exactly why the
integration suite drives one.

### The part that makes TDS different

**The TLS handshake rides inside PRELOGIN-typed TDS packets, and the stream
switches to raw TLS framing the moment the handshake completes.** None of the
other four proxies do this: MySQL and PostgreSQL upgrade on a clean byte
boundary, and MongoDB is TLS-from-byte-zero.

`handshakeConn` is the `net.Conn` handed to `crypto/tls`. It has three states:

```
framed      — reads/writes wrapped in PRELOGIN packets     (the handshake)
passthrough — reads/writes straight to the socket          (TLS records)
reverted    — reads/writes straight to the socket, no TLS  (ENCRYPT_OFF, post-login)
```

The one rule that matters: **an open outbound packet is finished (with EOM)
before any read.** A TLS flight is exactly "everything written since the last
read"; `crypto/tls` never says where a flight ends, but it does say when it
wants to read one back, which is the same boundary. Getting this wrong means
the peer waits forever for a packet sitting in a buffer — an opaque client
hang, not an error. `deactivate()` also flushes anything still pending, so a
handshake whose last action is a write cannot deadlock either.

### Encryption negotiation

The full matrix, applied to the client's ENCRYPTION byte:

| Client | TLS disabled on the listener | TLS available |
|--------|------------------------------|---------------|
| `ENCRYPT_OFF` (0) | answer `NOT_SUP`, cleartext | answer `OFF`, **login packet only** |
| `ENCRYPT_ON` (1) | answer `NOT_SUP`, **refuse** | answer `ON`, whole session |
| `ENCRYPT_REQ` (3) | answer `NOT_SUP`, **refuse** | answer `REQ`, whole session |
| `ENCRYPT_NOT_SUP` (2) | answer `NOT_SUP`, cleartext | answer `NOT_SUP`, cleartext |

The `ENCRYPT_OFF` row is the one that surprises everyone: **a client connecting
with `Encrypt=no` still performs a complete TLS handshake.** TLS then covers
the LOGIN7 packet and nothing else — both ends drop back to cleartext TDS as
soon as the login packet is through. dbbat implements that revert
(`revertibleConn`), so `Encrypt=no` clients see exactly what they would from a
real SQL Server.

A refusal is always reported *after* the PRELOGIN response, so the client
learns why rather than watching the socket drop.

### TLS version

The encapsulated handshake is pinned to **TLS 1.2**. Under 1.2 each side's
handshake ends on a *read*, which puts the framed→raw switch on the same byte
for both peers. Under TLS 1.3 the client's handshake ends on a *write*, and
drivers disagree about whether that final flight is still encapsulated — a
disagreement that presents as a hang rather than an error. Every SQL Server
client speaks TLS 1.2, so v1 pins it.

`DBB_MSSQL_TLS_DISABLE`, `DBB_MSSQL_TLS_CERT_FILE` and `DBB_MSSQL_TLS_KEY_FILE`
mirror `DBB_PG_TLS_*`, `DBB_MYSQL_TLS_*` and `DBB_MONGO_TLS_*`, including the
self-signed certificate generated at startup when no files are given.

## LOGIN7

`internal/proxy/mssql/login7.go`.

LOGIN7 (`0x10`) is a 94-byte fixed header followed by the blobs its
offset/length table points at:

- 36 bytes of scalars (declared length, TDS version, packet size, client
  program version, PID, connection id, four flag bytes, timezone, LCID);
- nine `offset/length` pairs at bytes 36–71 — host name, user name, password,
  app name, server name, extension, client library, language, database;
- a 6-byte client id (MAC address) at 72–77;
- three more pairs at 78–93 — SSPI, attached DB file, change password.

The lengths are **character** counts for the UCS-2LE fields and **byte** counts
for the extension and SSPI blobs. Mixing the two up is the classic LOGIN7 bug.

`FEATUREEXT` is not a blob in the table: when `OptionFlags3 & 0x10` is set, the
extension pair points at a DWORD which itself holds the offset of a block at
the end of the payload. The block is a run of `{feature id, length(DWORD),
data}` entries closed by `0xFF` — the same shape as the `FEATUREEXTACK` the
server answers with. dbbat carries the block through byte for byte so a
re-serialized login does not silently lose features (UTF-8 support, column
encryption, session recovery) the client asked for, and reads it only far
enough to refuse `FEDAUTH` (see [what is deliberately
unsupported](#what-is-deliberately-unsupported-in-v1)).

### Password obfuscation

The password field is scrambled, not encrypted: each UCS-2LE byte has its
nibbles swapped and is then XORed with `0xA5`. It offers no protection at all —
it exists only so a password is not literally legible in a packet capture. Note
it is **not** an involution: applying the transform twice gives `b ^ 0xFF`, so
scramble and descramble are separate functions.

**The descrambled password is never logged, at any level.** `Login7.String()`
omits it, nothing formats the struct with `%v`, and a test asserts it.

`serialize()` rebuilds a wire-identical payload from the parsed struct. The
upstream leg uses it to replace the client's credentials with the stored ones
and replay the login — see below.

## Client authentication

`internal/proxy/mssql/auth.go`.

**SQL authentication only.** The username and password come out of LOGIN7 and go
through the same path every other proxy uses: an Argon2id check against the
dbbat user (via `internal/cache`, so a hot reconnect loop does not re-derive the
hash every time), or a `dbb_` API key in the password slot.

The **database** field of LOGIN7 names the **dbbat entry**, not a database on the
target — the same convention as the other four proxies. The real database name
comes from the server row. A row whose protocol is not `mssql` is refused, so a
name collision cannot be used to reach a PostgreSQL entry through the TDS
listener. The user must also hold a live grant on that entry.

Every refusal is a proper TDS `ERROR` + `DONE` stream, so a driver surfaces the
reason instead of a dropped socket:

| Cause | Number | What the client is told |
|-------|--------|-------------------------|
| Unknown user, wrong password, unknown/foreign API key | 18456 | `Login failed for user '<name>'.` — **identical wording for all of them** |
| No database named in the login | 4060 | connect with the name of a dbbat entry |
| No such entry, or an entry on another protocol | 4060 | the Database is the dbbat entry's name |
| No active grant | 4060 | request access in the dbbat UI |
| Upstream connect failed | 50000 | the proxy authenticated you; the target is the problem |

The text is derived from the *class* of failure only, never from the underlying
error, so store errors, upstream hostnames and driver internals never cross the
client leg.

## The upstream leg

`internal/proxy/mssql/upstream.go`, with the `ssl_mode` policy in
`internal/proxy/upstream/mssql.go`.

dbbat opens its **own** TDS session on the target: PRELOGIN, optionally the
encapsulated TLS handshake, then a LOGIN7 built from the *client's* login with
the credentials swapped for the stored (decrypted) ones.

Replaying the client's own login is deliberate. Everything that decides the
*shape* of the token stream coming back — TDS version, packet size, option
flags, collation, the `FEATUREEXT` block — is kept, because dbbat forwards that
response to the client untouched; negotiating anything different upstream would
hand the client a stream it did not ask for. The host name and client id are
kept too, so `sys.dm_exec_sessions` shows the real originating machine. What is
dropped: integrated security and any SSPI blob, a `FEATUREEXT` block that asks
for federated auth or that does not decode (already refused on the client leg —
dropped here as belt and braces), a password change, and an attached database
file. `AppName` becomes `dbbat/<version> @<user> for <the
client's own app name>`.

The client's login response is the upstream's own `LOGINACK` / `ENVCHANGE` /
`INFO` / `DONE` stream, relayed verbatim — the client sees the real server it is
talking to, not something dbbat invented.

### ssl_mode on the upstream leg

The two legs negotiate encryption **independently**: a plaintext client can sit
in front of an encrypted upstream and vice versa. The upstream side reads the
same `Plan` every other protocol reads (`upstream.PlanFor`), mapped onto a
PRELOGIN `ENCRYPTION` offer:

| Attempt | Offered | Accepted answers |
|---------|---------|------------------|
| encrypted | `ENCRYPT_ON` | `ON`, `REQ` |
| plaintext | `ENCRYPT_NOT_SUP` | `NOT_SUP` |

`ENCRYPT_OFF` is never offered upstream: it encrypts the login packet and then
reverts, which is neither of the two outcomes `upstream_tls` can honestly
report. The check is strict in both directions — under `require` a server that
will not encrypt is a failure, and under `disable` a server that insists on
encrypting is one too.

TDS settles encryption in PRELOGIN before a byte of login is sent, so an
opportunistic mode (`prefer`, `allow`) redials rather than renegotiating. Only a
transport problem — an encryption mismatch or a failed TLS handshake — allows
the next attempt; a **rejected login ends the chain**, because retrying a
refused password over a different transport would be a downgrade triggered by
the wrong signal.

The outcome lands on the connection row's `upstream_tls`, so the connections UI
reports SQL Server sessions like every other protocol.

`internal/proxy/conncheck` runs this exact connector (`mssql.ConnectUpstream`)
as its SQL Server probe, so a green connectivity check means the code path a
real session takes got in. Like MongoDB's, the connector lives in the protocol
package rather than in `internal/proxy/upstream`, because it is built on this
package's codec and `upstream` must not import a protocol package.

## The relay

`internal/proxy/mssql/relay.go`.

Once both legs are up, the two directions run as **independent pumps** rather
than a request/response loop. That is what makes `ATTENTION` (`0x06`) work: a
client cancelling a query sends it while the response is still streaming, and a
lock-step relay would not read it until the response it is meant to interrupt
had finished.

The two directions are deliberately asymmetric:

- **client → upstream**: whole logical messages. Interception has to happen
  here, and a hook that saw packet fragments could be evaded by splitting a
  statement across a packet boundary. The 16 MB reassembly cap bounds the cost.
- **upstream → client**: one packet at a time. A result set is a *single*
  logical TDS message and can be arbitrarily large; reassembling it would blow
  that cap and make the proxy buffer a whole result before the client sees its
  first row.

Each codec ends up with one reader and one writer in different goroutines, which
is safe because its read and write paths share no mutable state.

`RESETCONNECTION` (status bit `0x08`, and its `SKIPTRAN` variant) is carried
through onto the forwarded message. A connection pool sets it on the first
packet of a reused connection to ask for a clean session; dropping it would
leave the upstream carrying the previous logical session's temp tables and SET
options — state the same client would not see connecting directly.

The two pumps call one hook each — `clientMessageHook` for interception,
`serverPacketHook` for result accounting. The request hook returns the payload
to forward; **a nil return means the hook answered the client itself**, which is
how a blocked statement never reaches the upstream. ATTENTION is a message with
*no payload at all*, so a hook must hand back a non-nil empty slice for it —
returning its own argument would silently swallow every client cancellation.

Because a refusal is written on the *client* codec from the request pump, while
the response pump is also writing there, the session serializes client writes
behind one mutex. `packetRW` is explicitly not safe for concurrent use: its
outbound packet id is shared state.

## Query interception

`internal/proxy/mssql/intercept.go`, `internal/proxy/mssql/rpc.go`.

Every complete client message is classified before anything is forwarded. The
request pump hands the hook **whole logical messages**, not packets, so a
statement split across a packet boundary cannot slip past.

| TDS message | Treatment |
|-------------|-----------|
| `SQLBatch` (`0x01`) | statement decoded from UCS-2LE, logged, **enforced** |
| `RPC` (`0x03`) | every request in the batch parsed; statement text extracted, logged, **enforced** |
| `BulkLoadBCP` (`0x07`) | refused under `read_only` or `block_copy` |
| `Attention` (`0x06`) | relayed untouched |
| `TransactionManager` (`0x0E`) | relayed untouched |
| anything else | relayed untouched |

The enforcement order is the one every other proxy uses: revocation → quotas →
the grant's static controls (`read_only`, `block_ddl`, `block_copy`,
password-change) → the approval gate. Only a statement that clears all four
reaches the upstream.

A refusal is answered as an `ERROR` (number 50000, class 16) followed by a
`DONE` with `DONE_ERROR` — the shape a real SQL Server statement error takes, so
the driver raises an ordinary SQL error and the connection stays usable for the
next statement.

### SQLBatch is UCS-2, not UTF-8

The statement text in a SQLBatch is UCS-2LE, behind an `ALL_HEADERS` block.
Reading those bytes as text would mangle every non-ASCII identifier, and — the
part that matters — could let a crafted statement slip past a pattern-based
control, which is a security control and not a display concern. The decode is
round-tripped in tests over accented, Cyrillic, Greek and surrogate-pair SQL.

`ALL_HEADERS` is validated, and a block that does not validate **refuses the
message** rather than being skipped. That is a security property, not tidiness:
skipping it would decode the header bytes as part of the statement, and every
grant control dbbat applies is prefix-based (`strings.HasPrefix(upper,
"DELETE")` and friends). A garbage prefix therefore turns `DELETE FROM t` into
something `read_only` no longer matches, while the upstream may still recognize
the statement. `ALL_HEADERS` is mandatory from TDS 7.2 and the proxy's floor is
7.4, so a client that omits or mangles it is a red flag, not a compatibility
case — refusing costs nothing.

### RPC is enforced, not log-only

**This is the owner's binding decision, and it is a security property.**
`read_only` and `block_ddl` are access controls; a log-only RPC path would let
any client bypass them by wrapping a write in `sp_executesql`. There is an
integration test that issues the identical `DELETE` twice — once as a plain
`SQLBatch`, once through `sp_executesql` — and asserts both are refused.

Enforcement runs on the **statement template**. Parameter values are decoded and
captured for the query row, but they are not what the controls match, which is
the right granularity: `read_only` is about what the statement *does*.

SQL Server accepts these system procedures both positionally and by name, so
dbbat enforces on **every candidate**: the documented positional slot, plus any
parameter explicitly named `@stmt` / `@statement` / `@tsql` / `@rpccall`. A
client that could get dbbat to validate one parameter while the server ran
another would have a `read_only` bypass; a normal driver call sends the
statement unnamed and positional, so there is exactly one candidate and nothing
changes.

dbbat parses both RPC forms — a procedure name, and the well-known
procedure-id shorthand every driver actually uses — plus multi-call batches
separated by a batch flag.

**Only the procedure-id shorthand is read for a statement.** A call that names
its procedure — including one that spells out `sp_executesql` — is treated as an
opaque stored procedure and fails closed under a restrictive grant, because the
name resolution and parameter conventions of an arbitrary procedure are not
something dbbat can assume. Drivers always send these by id. The forms that
carry SQL, and where:

| Form | id | Statement parameter |
|------|----|---------------------|
| `sp_executesql` | 10 | 0 (`@stmt`) |
| `sp_prepare` | 11 | 2 |
| `sp_prepexec` | 13 | 2 |
| `sp_cursorprepare` | 3 | 2 |
| `sp_cursoropen` | 2 | 1 |
| `sp_cursorprepexec` | 5 | 3 |
| `sp_prepexecrpc` | 14 | 1 |

### Prepared statements, and where dbbat fails closed

`sp_execute` (handle at parameter 0) and `sp_cursorexecute` (handle at parameter
0, the cursor OUT at 1) carry a **handle**, not SQL. dbbat resolves them: the
response accountant reads the `RETURNVALUE` token a prepare answers with and
remembers that handle against the statement text that was already validated, so
an execute is checked against the SQL it actually runs. The map is per session
and bounded — no handle enters it that dbbat did not watch being prepared.

A resolved execute is also **logged and pattern-matched as the statement it
runs**, not as `EXEC sp_execute 41`. That is what the MySQL proxy does for
`COM_STMT_EXECUTE`, and it is load-bearing: approval patterns match the logged
text, so recording the handle would make the four-eyes rule silently stop
applying the moment a client used prepared statements. Releasing a handle
(`sp_unprepare`) is *not* logged as its statement — it is not running it again.

Three cases cannot be resolved, and all three **fail closed whenever the grant
restricts anything** (`read_only`, `block_ddl` or `block_copy`) — and are
forwarded when it restricts nothing, because then there is nothing to fail
closed about:

- **An unknown handle.** A handle dbbat never saw prepared on this session names
  a statement it cannot see. (`ErrUnknownPreparedStatement`)
- **A stored procedure called by name.** `EXEC dbo.Something` is opaque: dbbat
  cannot see the body, so it cannot prove the procedure respects the
  restriction. (`ErrOpaqueProcedureBlocked`)
- **A request that will not parse.** An unparseable SQLBatch or RPC — including
  one whose `ALL_HEADERS` block does not validate — is refused rather than
  relayed, because it is one dbbat cannot enforce a grant on.
  (`ErrMalformedRequest`) Unlike the two above, this one fails closed under
  *every* grant, restrictive or not.

Cursor bookkeeping that carries neither a statement nor a decision —
`sp_cursor`, `sp_cursorfetch`, `sp_cursoroption`, `sp_cursorclose`,
`sp_cursorunprepare`, `sp_unprepare` — is allowed under a restrictive grant: the
statement it acts on was checked when it was prepared or opened.

### `block_copy` on SQL Server

`block_copy` is PostgreSQL's `COPY` control. Its SQL Server analogue is bulk
copy, so it matches `BULK INSERT`, `INSERT BULK` and `OPENROWSET(BULK …)`. The
`INSERT BULK` statement is the primary gate — a bulk-copy client announces it as
an ordinary SQLBatch — and the `BulkLoadBCP` (`0x07`) message that streams the
rows is refused as well, so no ordering trick delivers rows to the upstream. An
allowed bulk-load message still goes through the pipeline: the rows carry no
statement to check, but they do consume the grant's quota and belong in the
audit trail.

### Approval holds

Holds go through the shared gate (`docs/approvals.md`). While a statement is
parked the session goroutine is blocked, so the client conn is **parked**: a
watcher keeps reading the socket, which is what makes "the client went away" an
event rather than a silence. Bytes the client sends meanwhile are queued and
replayed in order once the session resumes.

One consequence is worth stating: an `ATTENTION` (the TDS cancel) sent *during*
a hold is queued like anything else, so it does not release the hold — it is
delivered to the upstream immediately after the released statement, cancelling
it there. A client that gives up and disconnects does end the hold, through the
watcher. See `specs/todos/2026-08-07-mssql-attention-during-approval-hold.md`.

## Result accounting

`internal/proxy/mssql/result.go`, `internal/proxy/mssql/tokens.go`,
`internal/proxy/mssql/typeinfo.go`.

The response hook walks the token stream: `COLMETADATA` (full `TYPE_INFO`,
including the BYTELEN / USHORTLEN / LONGLEN / PLP families), `ROW`, `NBCROW`,
`DONE` / `DONEPROC` / `DONEINPROC`, `ERROR`, `INFO` and `RETURNVALUE`. Every
other token is skipped by its length.

Three things about it are not obvious:

- **It has to be incremental.** Responses are forwarded a packet at a time, so
  tokens routinely straddle packet boundaries. The walker keeps a carry buffer
  with a hard cap; past the cap it gives up rather than growing without bound.
- **It is strictly observational.** Nothing it does can alter the bytes the
  client receives, so a stream it cannot follow is a lost *measurement*, never a
  broken session. An unmodelled token (`ALTMETADATA` / `ALTROW` from
  `COMPUTE BY`, or the Always Encrypted CEK table) desynchronises the walk for
  that message only.
- **There is a backstop.** The last token of any TDS response is a DONE-family
  token, so a 13-byte rolling tail recovers the row count even from a walk that
  gave up.

`DONE_COUNT` is what says the row-count field means anything; without it the
field is padding, and reading it anyway is how a proxy invents rows that never
existed. `DONEPROC` is deliberately not counted — it repeats the count of the
`DONEINPROC` inside the same procedure, so counting both would report every
`sp_executesql`'s rows twice.

An `ERROR` token becomes the query row's `error`, as a decoded diagnostic
("*message* (error N, state S, class C)"), and goes through
`shared.SanitizeQueryError` on the way — the repo has been burned before by a
decoder writing raw bytes into `queries.error`.

Captured rows are serialized as JSON arrays through the shared `RowWriter`, with
the same storage limits as the other protocols. Value decoding is best-effort by
design — the *framing* is what has to be exact — so a type the decoder does not
interpret is captured as a tagged base64 object (`{"$bytes": …, "$type": …}`),
the same shape the MySQL proxy uses for a binary blob.

`bytes_transferred` comes from a counting conn wrapped around the client socket,
below TLS, so it measures the whole client leg exactly as the MySQL and MongoDB
proxies do.

## What dbbat does not see

Plainly, so nobody assumes more coverage than there is:

- **A stored procedure's body.** dbbat sees `EXEC dbo.Something`, not what it
  does — hence failing closed under a restrictive grant. The same applies to a
  system procedure invoked by name rather than by its well-known id.
- **Rows inside a `BulkLoadBCP` stream.** The `INSERT BULK` statement that
  announces it is logged and checked; the rows themselves are relayed (or
  refused wholesale), not parsed.
- **Result sets behind an unmodelled token.** `COMPUTE BY` (`ALTMETADATA` /
  `ALTROW`) and Always Encrypted column metadata desynchronise the accountant,
  so those queries get a row count from the tail DONE and no captured rows.
- **Transaction Manager requests** (`0x0E`): `BEGIN` / `COMMIT` / `ROLLBACK`
  issued through the protocol rather than as SQL are relayed untouched and do
  not appear in the query history.
- **An ATTENTION during an approval hold**, as described above.

## What is deliberately unsupported in v1

- **MARS** (`MultipleActiveResultSets=True`) is refused in PRELOGIN. It
  multiplexes independent request streams over one connection, which the
  interception pipeline is not built for. Clients configured with it will fail
  to connect until they drop it.
- **Integrated authentication** — NTLM, Kerberos, SSPI. Refused with a message
  telling the user to connect with a SQL login.
- **Azure AD / Entra ID (federated) authentication**, refused in two places.
  dbbat declines it in PRELOGIN (`FEDAUTHREQUIRED = 0`), which is what makes
  every mainstream driver fall back to a SQL login; and `Login7.Validate()`
  refuses a LOGIN7 whose **FEATUREEXT** block still carries a `FEDAUTH` feature
  request (id `0x02`), with the same "connect with a SQL login" message as the
  integrated-security refusal. The second check is the one that matters: dbbat
  can neither mint nor validate a federated token, so relaying that request
  would start an exchange between client and server that the proxy has no part
  in — while sitting in the middle of it.
  A FEATUREEXT block that **does not decode** is refused too, rather than
  relayed: the block is where an authentication request lives, so one dbbat
  cannot read is one whose intent it cannot rule out. Every other feature in
  the block (session recovery, column encryption, UTF-8 support, Azure SQL
  support…) is relayed upstream byte for byte, because the upstream's
  FEATUREEXTACK is forwarded to the client untouched.
  `buildUpstreamLogin` drops the whole block in both refused cases as well —
  unreachable through a session, kept for the same reason the SSPI blob is
  stripped there.
- **Changing a password through the proxy.**
- **Pre-TDS7 logins** (packet type `0x02`).
- **TLS 1.3** on the client leg — see above.
- **TDS below 7.4.** The floor is **7.4 (SQL Server 2012+)**, enforced in
  `Login7.Validate()`. It is not arbitrary: the tokens the proxy emits use the
  TDS 7.2+ wide forms (a 4-byte ERROR line number, an 8-byte DONE row count),
  which an older client would misparse rather than cleanly reject. A LOGIN7
  with `TDSVersion = 0` means "server picks", which is 7.4 here.

## Session Packet Dumps

No change to the dump format is needed: pcapng is protocol-agnostic
(`dump.ProtocolMSSQL`). `DBB_DUMP_DIR` and friends work exactly as they do for
the other proxies, retention sweep included.

One difference worth knowing: the capture tap sits on the **codec**, not on the
socket. A socket-level tap on an encrypted client leg would record TLS records
and nothing legible; tapping the codec records the TDS packets themselves, in
plaintext, whatever the client leg negotiated. Only the post-auth stream is
captured, matching the other proxies.

## Testing

Unit tests are the gate for the handshake work, because a framing bug shows up
as a hang rather than a diff:

- `packet_test.go` — table-driven over synthesized bytes: split messages, a
  truncated trailer, a truncated header, a bad length field, mismatched packet
  types, the IGNORE bit, packet-size clamping, streaming-packet flushes.
- `tlsconn_test.go` — the adapter on its own (framing, unframing, the
  finish-on-read rule, pass-through after `deactivate`), plus a genuine
  `crypto/tls` handshake driven through two adapters over a `net.Pipe`.
- `prelogin_test.go` — option round-trip, the response shape, the whole
  encryption matrix.
- `login7_test.go` — parse/re-serialize round-trip, credential rewrite,
  FEATUREEXT survival, the FEDAUTH-in-FEATUREEXT refusal and its fail-closed
  twin (an undecodable block), the scramble in both directions with a pinned
  known vector, and the no-password-in-`String()` guard.
- `session_test.go` — a synthetic client over a real TCP listener through every
  encryption mode, decoding the ERROR/DONE tokens the way a driver would.
- `tokens_test.go` — the login-response token walker: a good login behind
  ENVCHANGE/INFO/FEATUREEXTACK/SESSIONSTATE tokens, a rejected one, an
  unmodelled token stopping the walk, and every truncation of a real stream.
  It also pins the token *numbers*: stage 2 had FEATUREEXTACK at `0xEE`, which
  is FEDAUTHINFO — and since FEATUREEXTACK precedes LOGINACK, a client asking
  for any feature extension would have had a perfectly good login read as a
  refusal.
- `typeinfo_test.go` — the TYPE_INFO and value framing, type by type, plus the
  value rendering. The framing assertions are the load-bearing ones: a wrong
  length loses the token stream, a wrong interpretation only makes a cell ugly.
- `rpc_test.go` — the UCS-2 SQLBatch round trip over accented, Cyrillic, Greek
  and surrogate-pair SQL; statement extraction for every RPC form that carries
  one; the by-name form; the handle forms; multi-call batches; and a truncated
  request never yielding a statement.
- `intercept_test.go` — the enforcement decisions as a table (including the
  fail-closed cases and the malformed-`ALL_HEADERS` prefix evasion), then
  end-to-end through the proxy: the same write refused as a SQLBatch *and*
  through `sp_executesql`, the session surviving a refusal, a statement logged
  with its row count and captured rows, an upstream ERROR landing on the query
  row, an approval hold parked and released mid-session, and a hold firing on a
  statement run through a prepared handle.
- `result_test.go` — the accountant against synthesized responses: rows and
  counts, NBCROW, every packet split point, an ERROR token, DONEPROC not being
  double-counted, the tail-DONE backstop, the RETURNVALUE handle, and the
  capture limits.
- `fakeupstream_test.go` — a fake TDS **server** on the same codec. It is what
  lets the whole upstream leg run under `make test` on any architecture, which
  matters because the real SQL Server image is linux/amd64 only.
- `upstream_test.go` — every `ssl_mode` path against that fake, including both
  redial directions, the two refusals (`require` will not downgrade, `disable`
  will not upgrade), and a rejected login ending the chain.
- `auth_test.go` — the auth path against a throwaway PostgreSQL store: valid
  user, wrong password, unknown user, API key, someone else's API key,
  integrated security, unknown/empty/wrong-protocol database, missing grant, and
  an upstream that refuses the stored credentials. The indistinguishability of
  the authentication failures is asserted on the wording, not just the number.
- `relay_test.go` — a request forwarded byte for byte, a response larger than
  one packet, an ATTENTION, the connection row closing on disconnect, and a
  capture file being written.

`integration_test.go` (build tag `integration`) starts an
`mcr.microsoft.com/mssql/server` container and drives the proxy with
`github.com/microsoft/go-mssqldb`: a real driver completes the handshake in each
encryption mode, then authenticates against a seeded dbbat user and runs real
`SELECT`s through to the container, with the connection row's `upstream_tls`
asserted for both a plaintext and an encrypted upstream leg. Stage 3 adds three
more: a `read_only` grant refusing the same write on both statement paths (the
driver sends a parameterless statement as a SQLBatch and a parameterised one as
`sp_executesql`, so this really does exercise both), row counts and captured
rows against a real result set, and an approval hold parked and then released
mid-session. `make test` neither compiles nor runs it:

```bash
make test-integration-mssql
```

| Variable | Purpose |
|----------|---------|
| `MSSQL_TEST_IMAGE` | Upstream SQL Server image (default `mcr.microsoft.com/mssql/server:2022-latest`) |

Microsoft publishes that image for **linux/amd64 only**, so on Apple Silicon it
runs under emulation if it runs at all. The CI job (`ubuntu-24.04`, in
`.github/workflows/integration.yml`) is where it is expected to pass.
