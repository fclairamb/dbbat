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
