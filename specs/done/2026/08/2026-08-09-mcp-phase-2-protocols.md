# MCP phase 2: Oracle, MongoDB and SQL Server

No GitHub issue yet — file one when picking this up.

## Goal

Let the MCP endpoint (`POST /api/v1/mcp`, `internal/mcp/`) run statements
against the three protocols phase 1 left out: Oracle, MongoDB and Microsoft SQL
Server. Today a grant on one of those appears in `list_databases` with
`supported: false`, and `query` refuses with `ErrProtocolUnsupported`.

## Why

Phase 1 shipped PostgreSQL and MySQL/MariaDB because their client libraries
were already in `go.mod`. The other three are the ones a lot of dbbat's value
sits behind — Oracle especially, where an agent that can read a legacy schema
without a human copying rows around is worth a great deal, and where the
approval gate matters most.

Nothing about the governance model changes: this is three loopback clients and
a switch case.

## Implementation

The constraint that governs everything here is the one stated in
`internal/mcp/doc.go` and `docs/mcp.md`: **statements are executed by dialing
dbbat's own proxy listener over loopback as the API key's owner**, never by an
internal path. Each protocol below means one new file next to
`exec_postgresql.go` / `exec_mysql.go`, one case in `LoopbackExecutor.Execute`,
one entry in `SupportedProtocol`, and nothing else — no auth, no grant check,
no approval plumbing, no new enforcement.

### Oracle

- Client: `github.com/sijms/go-ora/v3`, already a dependency (used by the
  proxy's own upstream leg and its tests). Connect with the EZ-Connect shape
  `BuildConnectionURL` already produces: `user/key@host:port/<dbbat db name>`
  against `config.ListenOracle`.
- The proxy's Oracle session resolver tries an exact `GetServerByName` lookup
  first, so pass the dbbat database name, not the upstream service name — see
  the comment in `internal/api/connection_url.go`.
- Row cap: `go-ora` exposes `database/sql` semantics, so read at most
  `MaxRows+1` and close.
- Watch out for `docs/approvals.md`'s Oracle caveats: a cursor re-execution is
  gated, an undecodable frame is not. Nothing to do about it here, but the MCP
  doc's per-protocol table should say so rather than implying parity.

### MongoDB

- Client: `go.mongodb.org/mongo-driver/v2`, already a dependency.
- Connect against `config.ListenMongo` with the URI shape
  `BuildConnectionURL` builds: `authMechanism=PLAIN`, `authSource=<dbbat db
  name>`, `tls=true`, `directConnection=true`. Note this one is **not**
  plaintext — the Mongo proxy terminates TLS and PLAIN needs it — so the
  loopback client has to accept the proxy's self-signed certificate
  (`tlsInsecure` equivalent, scoped to the loopback dial only, and commented as
  such).
- `query` takes SQL today. Mongo has none, so the tool surface needs a decision:
  either accept a command document (`{database, command: {...}}`) through a
  separate `run_command` tool, or let `sql` carry `<command> <extJSON>` — the
  same rendered text `/queries` shows and approval patterns match against. The
  second keeps the surface small and keeps patterns meaningful; prefer it
  unless it reads badly in practice.

### SQL Server

- Client: `github.com/microsoft/go-mssqldb`, already a dependency.
- Connect against `config.ListenMSSQL`. The TDS proxy answers
  `ENCRYPT_NOT_SUP` when `DBB_MSSQL_TLS_DISABLE` is set, and otherwise
  encapsulates a TLS handshake; the loopback client should ask for encryption
  `disable` if the proxy allows it, else accept the self-signed cert.
- `docs/mssql.md` notes the client leg's TLS ceiling
  (`DBB_MSSQL_TLS_MAX_VERSION`, 1.2 by default, 1.3 verified against
  `go-mssqldb` only) — which is the very client this would use, so 1.3 is
  reachable here.

### Shared work

- `internal/mcp/exec.go`: extend `SupportedProtocol` and the dispatch switch.
  `TestSupportedProtocol` in `exec_test.go` lists every protocol explicitly so
  that adding one is a deliberate edit there, not a silent side effect.
- `internal/mcp/tools.go`: `introspectionSQL` needs a case per protocol
  (`ALL_TAB_COLUMNS` for Oracle, `listCollections` for Mongo,
  `INFORMATION_SCHEMA.COLUMNS` for SQL Server). Table names must stay bind
  parameters — the input comes from a model.
- `docs/mcp.md` and `website/docs/features/mcp.md`: update the protocol tables.
- Integration coverage belongs with the protocol suites that already stand up
  real containers (`make test-integration-mssql`, `make test-integration-mongodb`,
  `make test-e2e-oracle`) rather than in `internal/mcp`, whose tests are
  deliberately database-free.

## Key files

- `internal/mcp/exec.go` — dispatch and `SupportedProtocol`
- `internal/mcp/exec_postgresql.go`, `exec_mysql.go` — the two clients to model
  the new ones on
- `internal/mcp/tools.go` — `introspectionSQL`
- `internal/api/connection_url.go` — the canonical connect-string shape per
  protocol
- `docs/mcp.md` — the protocol table and the per-protocol caveats
