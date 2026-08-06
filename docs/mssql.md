# SQL Server Proxy — Protocol Notes

DBBat is growing a proxy for Microsoft SQL Server, so that queries against SQL
Server get the same observability, grants, quotas and approval holds as the
PostgreSQL, Oracle, MySQL and MongoDB targets.

The listener defaults to `:1434` (`DBB_LISTEN_MSSQL`; empty disables it). The
`+1` convention the other proxies use — upstream port plus one — works here
because the service that owns 1434 upstream, the SQL Server Browser, is
**UDP-only**; 1434/tcp is free.

> **Status: stage 1 of 3.** What is implemented today is the TDS handshake and
> nothing behind it. A client completes PRELOGIN, the encapsulated TLS
> handshake and LOGIN7, and then receives a TDS error saying the proxy is not
> wired through to an upstream yet. Stage 2 adds authentication and the
> upstream leg; stage 3 adds query interception and result accounting.

## Library Choice

No Go library offers a TDS *server*, so the wire protocol is hand-rolled, like
the Oracle and MongoDB proxies. `github.com/microsoft/go-mssqldb` is pulled in
as a **test** dependency only, to drive the integration suite with a real
third-party client.

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

`serialize()` rebuilds a wire-identical payload from the parsed struct. Stage 2
uses it to replace the client's credentials with the upstream ones and replay
the login.

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

The minimum TDS version targeted is **7.4 (SQL Server 2012+)**; that is what
the proxy advertises in PRELOGIN.

## Session Packet Dumps

No change to the dump format is needed: pcapng is protocol-agnostic. Captures
land in stage 2, alongside the upstream leg they would record.

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

`integration_test.go` (build tag `integration`) starts an
`mcr.microsoft.com/mssql/server` container and drives the proxy with
`github.com/microsoft/go-mssqldb`, proving a real driver completes the
handshake in each encryption mode and reads back the stub error as an ordinary
SQL error. `make test` neither compiles nor runs it:

```bash
make test-integration-mssql
```

| Variable | Purpose |
|----------|---------|
| `MSSQL_TEST_IMAGE` | Upstream SQL Server image (default `mcr.microsoft.com/mssql/server:2022-latest`) |

Microsoft publishes that image for **linux/amd64 only**, so on Apple Silicon it
runs under emulation if it runs at all. The CI job (`ubuntu-24.04`, in
`.github/workflows/integration.yml`) is where it is expected to pass.
