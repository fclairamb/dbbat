---
model: opus
effort: high
---

# SQL Server proxy, stage 2: client auth, upstream connection and end-to-end relay

Stage 2 of 3, split from the original `2026-08-06-05-sql-server-proxy-support.md` on the
owner's instruction. **Depends on stage 1**
([05a](2026-08-06-05a-mssql-tds-framing-and-handshake.md)): the TDS packet codec, the
PRELOGIN/TLS handshake and the LOGIN7 parse/re-serialize must already exist. Stage 3
([05c](2026-08-06-05c-mssql-interception-and-results.md)) depends on this one.

## Goal

Turn stage 1's handshake-and-stop listener into a working proxy: authenticate the client
against DBBat's own users and API keys, resolve the target database, dial upstream with the
stored credentials, and relay the session. After this stage a `sqlcmd` or `go-mssqldb`
client can run a query through DBBat and get real results back — with **no** query
interception, grant enforcement or result accounting yet, which is stage 3.

## Proposal

### Client authentication

Stage 1 parses LOGIN7 into a struct. Take the username and password from it and authenticate
against DBBat exactly as the other proxies do, through `internal/cache` — see
`internal/proxy/mysql/auth.go` for the closest structural template. SQL authentication only:
NTLM, Kerberos and Azure AD are out of scope for v1 and must be rejected with a clear,
well-formed TDS error rather than an obscure disconnect.

The database is selected by the LOGIN7 database field, following whatever convention the
other proxies already use to map a client-supplied database name onto a DBBat database row —
read `internal/proxy/mysql/` and `internal/proxy/mongodb/` (which does an `authSource`
lookup) and stay consistent rather than inventing a fifth convention.

Authentication failures must be indistinguishable to the client between "no such user" and
"wrong password", as elsewhere in the codebase.

### Upstream connection

- `internal/proxy/upstream/mssql.go`: a connector matching the other files in that package,
  so `internal/proxy/conncheck/` can dial a SQL Server target and classify failures the same
  way it does for the existing four. Honour the `ssl_mode` policy that lives in that package
  — do not reimplement it.
- Prefer `microsoft/go-mssqldb` for the upstream leg **if** its internals are usable at the
  wire level from outside the package. If they are not, hand-roll the client side using
  stage 1's codec — the Oracle proxy is the precedent for that. Decide by reading the
  library first, and state which you chose and why in the commit body; do not add a heavy
  dependency you then only use for a fraction of the handshake.
- Re-open the upstream with the stored database credentials (decrypted through the existing
  path — never log them), replaying a LOGIN7 built from stage 1's serializer.
- The upstream leg negotiates its own encryption independently of the client leg, per the
  `ssl_mode` policy. Record the outcome on the connection row via the existing
  `upstream_tls` field, so the connections UI reports SQL Server sessions like the others.

### Session plumbing

- `internal/proxy/mssql/session.go` following the shape of `mysql/session.go`: connection
  row lifecycle, the shared dump writer (`dump.NewWriter` plus the blob uploader's
  `Finish` on close — copy the pattern from the four existing `session.go` files, including
  the nil-safe uploader), and clean teardown on either side closing.
- Relay both directions once the handshake completes. Stage 3 inserts interception into this
  relay, so keep the read loop structured so a hook can be added without rewriting it.

### Tests

- Unit tests for the auth path (valid user, bad password, unknown user, API key, an
  unsupported auth type) against a fake upstream.
- An integration test behind `//go:build integration` using testcontainers with
  `mcr.microsoft.com/mssql/server`, extending stage 1's `make test-integration-mssql` target:
  connect through the proxy, run a trivial `SELECT`, assert the rows arrive and a connection
  row was written with the expected `upstream_tls` value. `make test` must keep skipping it.

## Resolved open questions

Inherited from the owner's answers of 2026-08-06 — see
[05a](2026-08-06-05a-mssql-tds-framing-and-handshake.md) for the full set. The ones binding
on this stage:

- **Auth scope → SQL authentication only.** NTLM/Kerberos/Azure AD rejected with a clear
  error.
- **Minimum TDS version → 7.4 (SQL Server 2012+).**

No GitHub issue exists yet — one should be filed.

## Implementation Plan

Added by the implementing agent on 2026-08-07, before writing code.

### Library decision (read first, then decided)

`microsoft/go-mssqldb` v1.9.3 was read before choosing. Everything wire-level in
it is **unexported**: `tdsSession`, `tdsBuffer`, `writePrelogin`, `readPrelogin`,
`sendLogin`, `connect`, `getTLSConn` — the only exported surface is the
`database/sql` driver and `Connector`, which hands back a `driver.Conn` with no
access to the underlying socket or token stream. A proxy needs a raw
authenticated TDS connection it can relay packets over, which that API cannot
give. **Decision: hand-roll the client side on stage 1's codec** (the Oracle
proxy precedent). `go-mssqldb` stays a *test-only* dependency driving the
integration suite as a third-party client.

### Where the connector lives

`internal/proxy/upstream` must not import a protocol package (its own package doc
states the invariant), and the TDS client needs stage 1's packet codec,
PRELOGIN builder, LOGIN7 serializer and `handshakeConn`. So the split mirrors
the **MongoDB** carve-out already documented in `upstream/upstream.go`:

- `internal/proxy/upstream/mssql.go` — `MSSQLConfig`, the TDS PRELOGIN
  `ENCRYPTION` byte each `Attempt` maps to, and the retry predicate. The
  `ssl_mode` policy itself stays `PlanFor`; nothing is reimplemented.
- `internal/proxy/mssql/upstream.go` — `ConnectUpstream`, the actual dial +
  PRELOGIN + TLS + LOGIN7 + login-response classification.
- `conncheck` calls `mssql.ConnectUpstream` exactly as `probeMongo` calls
  `mongodb.ConnectUpstream`.

### Steps

1. `upstream/mssql.go` + the package-doc carve-out update.
2. `mssql/tokens.go` — a minimal TDS token-stream walker, enough to tell a
   successful login response from an `ERROR` one and to extract the message.
   Stage 3 grows it into the result accountant.
3. `mssql/upstream.go` — `UpstreamConn` (conn, revertible stream, packetRW,
   `TLS bool`, raw `LoginResponse`), the attempt loop over `PlanFor`, the client
   half of PRELOGIN, the encapsulated TLS handshake as `tls.Client` over stage
   1's `handshakeConn`, the LOGIN7 replay with the stored credentials, and the
   login-response read. The upstream's own login response is what gets forwarded
   to the client, so the client sees the real `LOGINACK`/`ENVCHANGE`.
4. `mssql/auth.go` — username/password (or `dbb_` API key) from the parsed
   LOGIN7 verified through `internal/cache`, database resolved from the LOGIN7
   database field by `GetServerByName` + protocol check (the convention all four
   proxies use), then `GetActiveGrant`. One indistinguishable failure message for
   unknown-user and bad-password.
5. `mssql/relay.go` — two pumps, like `mongodb/session.go`. Client→upstream reads
   whole logical messages (`ReadMessage`) so stage 3 has one hook point per
   request; upstream→client forwards **packet by packet** (a result set is one
   logical message and would blow the 16 MB reassembly cap), with the message
   boundary surfaced so stage 3 can accumulate.
6. `mssql/session.go` — replace the stub with: auth → grant → upstream connect →
   connection row (`WithUpstreamTLS`, `WithGrantUID`) → dump writer → relay, and
   nil-safe `dumpUploader.Finish` on close.
7. `mssql/server.go` + `main.go` — the store/auth-cache/dump/encryption-key
   dependencies and `SetDumpUploader`.
8. `conncheck/probes.go` — `probeMSSQL`.
9. `docs/mssql.md` — status, the upstream leg, the relay, the library decision.

### Out of scope (stage 3)

Query interception, `read_only`/`block_ddl`/quota enforcement per statement,
approval holds, the limit watchdog, result-row accounting and the row writer. The
relay is structured so the hook drops in; none of it is wired here.

### Tests

- `mssql/upstream_test.go` — a fake TDS upstream (pure Go, stage 1's codec) for
  every `ssl_mode` path, plus login rejection.
- `mssql/auth_test.go` — valid user, bad password, unknown user, API key, and an
  unsupported auth type, against the fake upstream and a throwaway PostgreSQL
  store (the `postgresql/copy_persist_test.go` precedent: testcontainers under
  `make test`).
- `mssql/tokens_test.go` — the token walker over synthesized streams.
- `mssql/integration_test.go` — `//go:build integration`: connect through the
  proxy, `SELECT`, assert rows and the connection row's `upstream_tls`. Authored
  but **not runnable on this arm64 host** (the SQL Server image is amd64-only);
  CI runs it.
