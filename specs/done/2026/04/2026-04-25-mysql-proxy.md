# MySQL Protocol Proxy — Umbrella Spec

> Initial GitHub issue: TBD (link when created)
>
> Architectural reference: `docs/mysql.md`

## Goal

Add MySQL wire-protocol support to DBBat, alongside the existing PostgreSQL and Oracle proxies. Same value proposition: transparent proxy for query observability, access control, and safety — but for MySQL clients connecting to MySQL upstreams.

At completion of all phases, a developer running `mysql -h dbbat-host -P 3306 -u dev -p mydb` will:
1. Authenticate against DBBat's user store (not upstream MySQL)
2. Have their grant checked (database, time window, controls, quotas)
3. Connect through to the upstream database
4. Have every query logged with full SQL text, timing, and result rows

## Why

- DBBat already supports two protocols (PG, Oracle). MySQL is the third most-requested.
- Unlike Oracle, MySQL's wire protocol is well-documented and a mature Go server library exists (`go-mysql-org/go-mysql`). This means the implementation is much closer in size to the PG proxy than the Oracle one.

## Scope (in)

- Proxy listener on a dedicated port (default `:3307` to avoid conflict with a co-located MySQL)
- Auth termination via `mysql_native_password` against the DBBat user store
- API key auth (`dbb_*` passwords) like the PG proxy
- Upstream connection using stored encrypted credentials
- Query interception (text protocol + prepared statements)
- Read-only / DDL / copy enforcement (regex-based, shared with PG/Oracle)
- MySQL-specific blocked operations: `LOAD DATA [LOCAL] INFILE`, `SELECT INTO OUTFILE/DUMPFILE`, replication and admin protocol commands
- Result row capture for text protocol (`COM_QUERY`)
- OpenAPI + frontend updates (protocol enum, port suggestions)
- Integration tests via testcontainers-go

## Scope (out — deferred)

- `caching_sha2_password` auth plugin (RSA key exchange) — v2
- TLS termination at the proxy (`SSL Request` accept) — v2
- Binary-protocol result row capture (`COM_STMT_EXECUTE` rows) — v2
- MariaDB explicit support (CI matrix, `ed25519` auth) — v2 if requested
- Stored procedure multi-result-set capture beyond the first — out
- Replication protocol passthrough — never (always blocked)

## Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Library | `go-mysql-org/go-mysql` | Mature, BSD, has both client and `server` subpackages. Vitess's lib pulls too many deps. |
| Auth | Termination, `mysql_native_password` only | Matches PG proxy UX. caching_sha2 needs RSA — defer. |
| TLS | Plaintext only (refuse `SSL Request`) | Matches PG proxy. Deploy on private network. |
| MariaDB | Out of scope | Untested; document as best-effort. |
| Read-only enforcement | Regex (shared) + DBA recommendation | MySQL session-level R/O is bypassable. |
| Default proxy port | `:3307` (configurable via `DBB_LISTEN_MYSQL`) | Avoids conflict with local MySQL on `:3306`. |
| Database `port` SQL default | Drop the `5432` default | Force protocol-aware port specification at the API layer. |
| Phasing | 3 phases (auth → intercept → rows) + UI + API specs | Matches Oracle phasing for reviewability. |

## Phases

### Phase 1 — Connection & Authentication
Spec: `2026-04-25-mysql-phase1-connection-auth.md`

A MySQL client can complete the handshake through DBBat to an upstream MySQL database. DBBat verifies credentials against its user store, checks grants, and connects to upstream with stored creds.

### Phase 2 — Query Interception
Spec: `2026-04-25-mysql-phase2-query-interception.md`

Every `COM_QUERY` and `COM_STMT_PREPARE`/`COM_STMT_EXECUTE` is logged. Read-only / DDL / copy controls are applied. MySQL-specific blocked operations refuse the query. Forward to upstream if allowed.

### Phase 3 — Result Capture
Spec: `2026-04-25-mysql-phase3-result-capture.md`

Text-protocol result rows are captured per the existing `query_storage` config (max rows, max bytes). Stored as JSONB in `query_rows` for the existing API and UI to render.

### UI changes
Spec: `2026-04-25-mysql-ui-protocol-support.md`

Protocol dropdown gains "MySQL". Port input shows protocol-aware default (3306). Database list/detail pages show the protocol icon.

### API changes
Spec: `2026-04-25-mysql-api-protocol-support.md`

OpenAPI `protocol` enum includes `mysql`. Request validation requires `port` (no SQL default fallback). Database response includes `mysql` protocol.

## Cross-Cutting Concerns

### Database model

No new MySQL-specific columns. Existing fields are sufficient:
- `host`, `port`, `database_name`, `username`, `password_encrypted`, `ssl_mode`, `protocol`

One migration: drop the SQL default on `databases.port` (was `5432`). API layer enforces presence and applies protocol-aware defaults in error messages.

### Configuration

New env var: `DBB_LISTEN_MYSQL` (default `:3307`, empty disables).

### Shared validation

`internal/proxy/shared/validation.go` gains:
- `ValidateMySQLQuery(sql, grant)` — runs `ValidateQuery` plus MySQL-specific blocked patterns
- `mysqlBlockedPatterns` — `LOAD DATA`, `INTO OUTFILE`, `INTO DUMPFILE`

Existing `IsWriteQuery` / `IsDDLQuery` already include the keywords MySQL needs (`INSERT`, `UPDATE`, `DELETE`, `DROP`, `TRUNCATE`, `CREATE`, `ALTER`, `GRANT`, `REVOKE`, `MERGE`). Add `REPLACE` (MySQL-specific upsert).

### Store

- `ProtocolMySQL = "mysql"` constant in `models.go`
- No new table; `databases.protocol` accepts `mysql`
- `Connection`/`Query`/`QueryRow` reused as-is

### main.go wiring

Mirror the Oracle pattern:
```go
mysqlServer, err := startMySQLProxy(ctx, cfg, dataStore, proxyAuthCache, logger)
if mysqlServer != nil { servers = append(servers, mysqlServer) }
```

## Risks

| Risk | Mitigation |
|------|-----------|
| `LOAD DATA LOCAL INFILE` exfiltration | Refuse the protocol-level 0xFB packet AND regex-block the SQL pattern. Document in `docs/mysql.md`. |
| Client demands `caching_sha2_password` and won't fall back | Document the workaround per-client (CLI flags, JDBC params). Treat as known limitation until v2. |
| `go-mysql-org/go-mysql` API breaks | Pin to a tagged release; vendor if needed. |
| MySQL session state leak between connections (we don't pool) | Same as PG — each client connection gets its own upstream connection. No pooling. |

## Out-of-Scope Items Tracked

These should land as follow-up specs after Phase 3 ships:
- `2026-XX-XX-mysql-caching-sha2-password.md`
- `2026-XX-XX-mysql-tls-termination.md`
- `2026-XX-XX-mysql-binary-protocol-row-capture.md`
- `2026-XX-XX-mariadb-compatibility.md` (if requested)
