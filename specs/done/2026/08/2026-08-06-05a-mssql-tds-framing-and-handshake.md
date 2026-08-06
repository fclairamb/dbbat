---
model: opus
effort: xhigh
---

# SQL Server proxy, stage 1: TDS framing, PRELOGIN/LOGIN7 and the encapsulated TLS handshake

Stage 1 of 3, split from the original `2026-08-06-05-sql-server-proxy-support.md` on the
owner's instruction. Stages 2 ([05b](2026-08-06-05b-mssql-auth-and-upstream.md)) and 3
([05c](2026-08-06-05c-mssql-interception-and-results.md)) depend on this one; implement it
first. Do not attempt their scope here.

## Problem

DBBat proxies PostgreSQL, Oracle, MySQL/MariaDB and MongoDB, but not Microsoft SQL Server.
Teams running SQL Server get none of the observability, grants, quotas, or approval-hold
machinery, even though the shared pipeline (`internal/proxy/shared/`) is protocol-agnostic
and already absorbs four protocols.

The hard part of TDS is not the token stream — it is the handshake. **The TLS handshake is
encapsulated inside PRELOGIN-typed TDS packets, and the connection then switches to raw TLS
framing once it completes.** None of the four existing proxies do mid-stream TLS this way;
MySQL and PostgreSQL upgrade on a clean boundary. That quirk is why this stage exists on its
own: it is the part most likely to go wrong, and it is worth landing and testing before any
auth or interception work is layered on.

## Scope of this stage

A `internal/proxy/mssql/` package that speaks enough TDS for a real client to complete a
handshake, plus the plumbing that makes `mssql` a first-class protocol elsewhere in the
system. **No upstream connection and no query interception** — after a successful handshake
the session closes with a clear, well-formed TDS error saying the proxy is not yet wired
through. Stage 2 replaces that stub.

## Proposal

### Protocol work

- **Framing**: TDS packets — 8-byte header (type, status, length, SPID, packet id, window)
  followed by payload; a logical message spans packets until the `EOM` status bit. Build a
  codec that reads and writes these, reassembling messages, with the packet-size negotiation
  PRELOGIN agrees on. This is the unit most worth testing in isolation: table-driven tests
  over synthesized byte streams, including split-across-packets messages and a truncated
  trailer.
- **PRELOGIN (type 0x12)**: parse and respond to the option list (VERSION, ENCRYPTION,
  INSTOPT, THREADID, MARS, TRACEID, FEDAUTHREQUIRED). Target **TDS 7.4 (SQL Server 2012+)**
  as the minimum version — decided, see Resolved open questions — and document it in
  `docs/mssql.md`.
- **TLS encapsulation**: implement the encapsulated handshake. TLS records ride inside
  PRELOGIN-typed TDS packets until the handshake finishes, after which the stream is raw
  TLS. The clean way to do this is a `net.Conn` adapter that frames writes into TDS packets
  and unframes reads while a flag is set, handed to `crypto/tls`, with the flag cleared once
  `Handshake()` returns — keep that adapter small and unit-test it directly, because a bug
  here manifests as an opaque client hang.
- **Encryption negotiation**: support the full `ENCRYPT_OFF` / `ENCRYPT_ON` / `ENCRYPT_REQ` /
  `ENCRYPT_NOT_SUP` matrix on the client leg. `DBB_MSSQL_TLS_DISABLE` /
  `DBB_MSSQL_TLS_CERT_FILE` / `DBB_MSSQL_TLS_KEY_FILE` mirror the existing listeners
  (`DBB_PG_TLS_*`, `DBB_MYSQL_TLS_*`, `DBB_MONGO_TLS_*`), including the auto-generated
  self-signed cert when no file is given.
- **MARS**: refuse in PRELOGIN for v1 — decided, see Resolved open questions.
- **LOGIN7 (type 0x10)**: parse the packet into a struct (username, password, database,
  hostname, app name, client interface, and the offset/length table that makes this format
  fiddly), and be able to re-serialize it — stage 2 rewrites the credentials and replays it
  upstream. Password fields are XOR-scrambled; implement and unit-test the descramble both
  ways. **Never log a descrambled password**, at any log level.

### Integration points

- `internal/config/config.go` (~:363): `ListenMSSQL` (`DBB_LISTEN_MSSQL`, default `:1434` —
  upstream default 1433 + 1, matching the +1 convention used by the other listeners; the SQL
  Server Browser conflict on 1434 is UDP-only, so the TCP port is free). Empty disables.
- `main.go` (~:605): start/stop wiring next to the MongoDB listener, following the same
  nil-guarded shape.
- `internal/store/`: add `mssql` to the database protocol enum, with a migration if the enum
  is constrained in SQL.
- `internal/api/openapi.yml` + `front/`: protocol option in the database create/edit forms,
  protocol badge/icon, default port hint (1433).
- `docs/mssql.md`: protocol notes — framing, the PRELOGIN/TLS encapsulation in particular,
  the negotiated TDS version, and what is deliberately unsupported in v1. Root `CLAUDE.md`
  env-var table and connection-flow list.
- The dump format needs no change: pcapng is protocol-agnostic.

### Tests

Unit tests are the gate for this stage — the packet codec, the TLS-adapter framing, the
PRELOGIN option round-trip, and LOGIN7 parse/re-serialize. Add an integration test behind
`//go:build integration` that stands up `mcr.microsoft.com/mssql/server` via testcontainers
and drives a real client far enough to prove the handshake completes and the stub error is
returned; a `make test-integration-mssql` target with `-timeout 40m` plus a job in
`.github/workflows/integration.yml` (mirroring the MongoDB entries). `make test` must keep
skipping it.

## Resolved open questions

Answered by the repository owner, 2026-08-06. Binding.

- **Minimum TDS version → 7.4 (SQL Server 2012+).** Document the choice in `docs/mssql.md`.
- **MARS → refuse it in PRELOGIN for v1.** Clients configured with
  `MultipleActiveResultSets=True` will fail to connect until they drop it; say so in
  `docs/mssql.md` so the failure is diagnosable.
- **RPC grant enforcement → enforce, not log-only** (lands in stage 3, recorded here so the
  stages stay consistent). `read_only` and `block_ddl` are security controls; a log-only RPC
  path lets any client bypass them by wrapping a write in `sp_executesql`.
- **Auth scope → SQL authentication only.** NTLM, Kerberos and Azure AD are out of scope for
  v1; reject them with a clear error rather than failing obscurely.

No GitHub issue exists yet — one should be filed.

## Implementation Plan

Written before the code, revised as it landed.

### 1. `internal/proxy/mssql/packet.go` — TDS framing

`packetHeader` (type, status, length, SPID, packet id, window) with `encode`/
`decodeHeader`, and a `packetRW` bound to a `net.Conn` (actually an
`io.ReadWriter`, so tests drive it over `bytes.Buffer` / `net.Pipe`):

- `ReadMessage()` — reassemble packets until the `EOM` status bit, returning
  `(packetType, payload)`. Enforces a max reassembled size so a peer cannot
  make the proxy allocate without bound.
- `WriteMessage(typ, payload)` — split at the negotiated packet size, `EOM`
  only on the last packet, packet id incrementing mod 256.
- Streaming variants (`beginPacket` / `Write` / `finishPacket`) for the TLS
  adapter, which cannot know a flight's length up front.
- `SetPacketSize` for the size LOGIN7 asks for.

Table-driven tests over synthesized bytes: single packet, message split across
packets, truncated header, truncated payload (trailer), bad length field,
oversize message, packet-id wraparound, round-trip at several packet sizes.

### 2. `internal/proxy/mssql/prelogin.go` — PRELOGIN

Option-list codec: 5-byte entries (token, offset BE16, length BE16) terminated
by `0xFF`, then the blobs at those offsets. Tokens VERSION(0), ENCRYPTION(1),
INSTOPT(2), THREADID(3), MARS(4), TRACEID(5), FEDAUTHREQUIRED(6), NONCEOPT(7).
`buildResponse` answers VERSION + ENCRYPTION + INSTOPT + THREADID + MARS(off,
i.e. refused) and echoes FEDAUTHREQUIRED(0) only when the client offered it.
`negotiateEncryption` implements the full OFF/ON/REQ/NOT_SUP matrix and returns
`(serverResponse, mode)` where mode ∈ {none, loginOnly, full} or an error.

### 3. `internal/proxy/mssql/tlsconn.go` — the encapsulated handshake

`handshakeConn` is a `net.Conn` over `packetRW`:

- `Write` lazily begins a PRELOGIN-typed packet and buffers into it.
- `Read` first *finishes* any pending packet (with `EOM`) — a TLS flight is
  complete exactly when the stack next wants to read — then serves bytes from
  the current inbound message, pulling the next message when exhausted.
- `deactivate()` flushes anything pending and flips to raw pass-through, so the
  same `tls.Conn` keeps working once `Handshake()` has returned.
- `revertToPlaintext()` for `ENCRYPT_OFF`, where only the login packet is
  encrypted and the stream goes back to cleartext TDS afterwards.

Unit-tested directly: framing of writes, unframing of reads, the finish-on-read
rule, pass-through after deactivate — plus a real `crypto/tls` handshake driven
through two `handshakeConn`s over a `net.Pipe`.

### 4. `internal/proxy/mssql/login7.go` — LOGIN7

Parse the 94-byte fixed header + the 9 offset/length pairs + SSPI/AtchDBFile/
ChangePassword, decode the UCS-2LE blobs, and descramble the password
(`swap-nibbles ^ 0xA5` on encode, `^ 0xA5` then swap on decode — not an
involution, so both directions are implemented and tested). `serialize()`
rebuilds a wire-identical packet from the struct so stage 2 can rewrite the
credentials and replay it. `String()`/logging never touches the password.

### 5. `internal/proxy/mssql/errors.go` — TDS error tokens

ERROR (0xAA) + DONE (0xFD) token builders in a type-0x04 response, so the stub
"not wired through yet" failure is a well-formed login rejection any client
surfaces as a normal error rather than a hang or a parse failure.

### 6. `internal/proxy/mssql/{tls,server,session}.go`

`tls.go` mirrors `mongodb/tls.go` (`DBB_MSSQL_TLS_*`, auto self-signed).
`server.go` mirrors the other proxies' listen/accept/shutdown. `session.go`
runs PRELOGIN → negotiate → optional encapsulated TLS → LOGIN7 → stub error.
Stage 1 deliberately takes no store/authCache dependency: there is nothing to
authenticate against yet, and the `unused` linter would flag dead fields.

### 7. Integration points

`internal/config/config.go` (`MSSQLConfig`, `ListenMSSQL` default `:1434`,
`mssql_tls_*` env prefix), `main.go` (`startMSSQLProxy` + `collectServers`),
`internal/api/parameters.go` + `openapi.yml` (`listen.mssql`, protocol enum),
`front/` (protocol option, label, port hint 1433, listener row),
`internal/store/models.go` (`ProtocolMSSQL`; the `protocol` column is plain
TEXT with no CHECK, so no migration), `docs/mssql.md`, root `CLAUDE.md`.

### 8. Tests

Unit: everything above, plus a session-level test that drives a synthetic
client through PRELOGIN/TLS/LOGIN7 over a real TCP listener and asserts the
stub error token comes back. Integration (`//go:build integration`):
`mcr.microsoft.com/mssql/server` via testcontainers + `make
test-integration-mssql` + a matrix entry in `.github/workflows/integration.yml`.
