# SQL Server Proxy — Protocol Notes

DBBat is growing a proxy for Microsoft SQL Server, so that queries against SQL
Server get the same observability, grants, quotas and approval holds as the
PostgreSQL, Oracle, MySQL and MongoDB targets.

The listener defaults to `:1434` (`DBB_LISTEN_MSSQL`; empty disables it). The
`+1` convention the other proxies use — upstream port plus one — works here
because the service that owns 1434 upstream, the SQL Server Browser, is
**UDP-only**; 1434/tcp is free.

> **Status: stage 2 of 3.** A client authenticates against dbbat's own users
> and API keys, dbbat opens its own session on the target with the stored
> credentials, and the session is relayed end to end — a `sqlcmd` or
> `go-mssqldb` client runs queries and gets real results. What is **not** here
> yet is stage 3: no statement is logged, no `read_only` / `block_ddl` /quota
> control is enforced per query, no approval hold can fire, and no result rows
> or byte counts are accounted for. Connection-level access control **is**
> enforced: a login without a live grant on the requested database is refused.

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
the end of the payload. dbbat carries that block through opaquely so a
re-serialized login does not silently lose features (UTF-8 support, column
encryption, session recovery) the client asked for.

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
dropped: integrated security and any SSPI blob, a password change, and an
attached database file. `AppName` becomes `dbbat/<version> @<user> for <the
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

Stage 3 fills the two hooks (`clientMessageHook`, `serverPacketHook`) that the
pumps already call; nothing about the loops has to change.

## What is deliberately unsupported in v1

- **MARS** (`MultipleActiveResultSets=True`) is refused in PRELOGIN. It
  multiplexes independent request streams over one connection, which the
  interception pipeline is not built for. Clients configured with it will fail
  to connect until they drop it.
- **Integrated authentication** — NTLM, Kerberos, SSPI. Refused with a message
  telling the user to connect with a SQL login. Azure AD / federated
  authentication is declined in PRELOGIN (`FEDAUTHREQUIRED = 0`).
- **Changing a password through the proxy.**
- **Pre-TDS7 logins** (packet type `0x02`).
- **TLS 1.3** on the client leg — see above.
- **TDS below 7.4.** The floor is **7.4 (SQL Server 2012+)**, enforced in
  `Login7.Validate()`. It is not arbitrary: the tokens the proxy emits use the
  TDS 7.2+ wide forms (a 4-byte ERROR line number, an 8-byte DONE row count),
  which an older client would misparse rather than cleanly reject. A LOGIN7
  with `TDSVersion = 0` means "server picks", which is 7.4 here.

## Ahead of this stage

- **Stage 3** — intercepting SQLBatch (`0x01`) and RPC (`0x03`), logging
  queries, enforcing `read_only` / `block_ddl` / quotas per statement, honouring
  approval holds, and accounting for result rows and bytes. **RPC is enforced,
  not log-only**: `read_only` and `block_ddl` are security controls, and a
  log-only RPC path would let any client bypass them by wrapping a write in
  `sp_executesql`.

Two consequences of stage 2 shipping without stage 3 are worth stating plainly:
a SQL Server session is **relayed but not observed** (nothing appears in the
query history), and a grant's per-statement controls and quotas have **no effect
on it**. Connection-level access control does apply — no grant, no session.

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
  FEATUREEXT survival, the scramble in both directions with a pinned known
  vector, and the no-password-in-`String()` guard.
- `session_test.go` — a synthetic client over a real TCP listener through every
  encryption mode, decoding the ERROR/DONE tokens the way a driver would.
- `tokens_test.go` — the login-response token walker: a good login behind
  ENVCHANGE/INFO/FEATUREEXTACK/SESSIONSTATE tokens, a rejected one, an
  unmodelled token stopping the walk, and every truncation of a real stream.
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
asserted for both a plaintext and an encrypted upstream leg. `make test` neither
compiles nor runs it:

```bash
make test-integration-mssql
```

| Variable | Purpose |
|----------|---------|
| `MSSQL_TEST_IMAGE` | Upstream SQL Server image (default `mcr.microsoft.com/mssql/server:2022-latest`) |

Microsoft publishes that image for **linux/amd64 only**, so on Apple Silicon it
runs under emulation if it runs at all. The CI job (`ubuntu-24.04`, in
`.github/workflows/integration.yml`) is where it is expected to pass.
