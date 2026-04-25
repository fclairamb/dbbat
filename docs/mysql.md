# MySQL Proxy — Protocol Notes

DBBat's MySQL proxy support, alongside the existing PostgreSQL and Oracle proxies. This document is the architectural reference: wire-protocol decisions, library choice, auth strategy, and known limitations.

For implementation history and per-phase specs, see `specs/done/2026/04/2026-04-25-mysql-*.md`.

## Library Choice

We use [`go-mysql-org/go-mysql`](https://github.com/go-mysql-org/go-mysql) (BSD-3) for both the **server** side (accept client connections, speak the wire protocol) and the **client** side (connect to upstream MySQL).

Why not roll our own (the Oracle approach)?
- The Oracle proxy is ~25 files (~3,500 lines) and still has known gaps — its TTC parser is reverse-engineered against real Oracle traffic.
- MySQL's wire protocol has more message types than Oracle's TTC and at least four auth plugins. Hand-rolling would multiply effort without any quality gain.
- `go-mysql-org/go-mysql` is mature (used by go-mydumper, MyDumper, several CDC tools), actively maintained, and exposes a `server` subpackage explicitly designed for proxy/server scenarios.

Why not Vitess `vitess.io/vitess/go/mysql`?
- Apache 2.0 and arguably the most battle-tested implementation, but pulls in a large dep tree (gRPC, etcd client, Vitess types). Not worth the bloat for our use case.

## Auth Strategy: Termination via `mysql_native_password`

DBBat **terminates** authentication on the proxy side, the same way the PostgreSQL proxy does. The client's password is verified against the DBBat user store (Argon2id hashes) — never against upstream MySQL. DBBat then re-authenticates to upstream using the database's stored, encrypted credentials.

### Auth plugin: `mysql_native_password` (only, for v1)

DBBat advertises `mysql_native_password` as the server's default auth plugin during the initial handshake. This is a SHA1-based challenge-response that all major MySQL clients support as a fallback.

**Why not `caching_sha2_password`** (the MySQL 8.0 default)?
- It requires either a TLS-encrypted connection or an RSA key exchange to transport the cleartext password. The RSA path means DBBat would need to maintain a public/private RSA keypair and implement OAEP padding for first-time auth.
- Most clients fall back gracefully when the server advertises a different plugin.
- v2 follow-up: implement `caching_sha2_password` with full RSA key exchange.

### Client compatibility

- **mysql CLI** (8.x): works out of the box; may print "auth method mysql_native_password is not the recommended one" — harmless.
- **MySQL Connector/J (JDBC)**: works; may need `allowPublicKeyRetrieval=true` to be explicit.
- **mysql2 (Node.js)**: works; may need `authPlugins: { mysql_native_password: ... }` if pinned to caching_sha2.
- **PyMySQL**: works.
- **DBeaver**: works against `mysql_native_password`-only servers.

### API key auth (proxy-side)

The PG proxy accepts DBBat API keys (prefix `dbb_`) as the password. The MySQL proxy does the same: when the proxy receives a password starting with the API key prefix, it verifies it as an API key instead of a user password.

## TLS Handling: Plaintext-Only (v1)

DBBat refuses the client's `SSL Request` packet during handshake. This matches the PostgreSQL proxy's current behavior. Deployments must put DBBat on a private network (VPN, VPC, kube-internal).

Upstream connections **may** use TLS — the existing `databases.ssl_mode` column controls this. The proxy honors the upstream SSL mode regardless of what the client sees.

v2 follow-up: terminate TLS at the proxy with a server cert, accept `SSL Request` from clients.

## Connection Flow

```
┌────────┐                    ┌───────────┐                    ┌──────────────┐
│ Client │                    │   DBBat   │                    │ MySQL upstream│
│(mysql) │                    │  (proxy)  │                    │              │
└───┬────┘                    └─────┬─────┘                    └──────┬───────┘
    │                               │                                 │
    │  1. (connect TCP)             │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │  2. Handshake v10             │                                 │
    │     (auth plugin: native)     │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  3. HandshakeResponse41       │                                 │
    │     (user, db, auth_resp)     │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │                               │  Look up user                   │
    │                               │  Look up database (by db_name)  │
    │                               │  Check active grant             │
    │                               │  Verify password (Argon2id)     │
    │                               │  OR verify API key              │
    │                               │                                 │
    │                               │  4. Connect to upstream         │
    │                               │     (using stored creds)        │
    │                               │──────────────────────────────>│
    │                               │  5. Upstream handshake +        │
    │                               │     auth complete               │
    │                               │<─────────────────────────────>│
    │  6. OK packet                 │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │      === Command phase ===                                      │
    │                               │                                 │
    │  COM_QUERY "SELECT ..."       │  intercept: log + validate      │
    │──────────────────────────────>│  forward                        │
    │                               │──────────────────────────────>│
    │                               │  result rows + EOF              │
    │  result rows + EOF            │  intercept: capture rows        │
    │<──────────────────────────────│<──────────────────────────────│
```

The proxy is transparent for command-phase traffic — packets are forwarded with inspection, never altered. Auth is fully terminated; the upstream sees DBBat as the client.

## Read-Only Enforcement

MySQL's session-level `SET SESSION TRANSACTION READ ONLY` only applies to the *next* transaction and is trivially bypassable. Two-layer defense:

1. **Regex inspection** in the proxy (shared with PG/Oracle): block `INSERT`, `UPDATE`, `DELETE`, `DROP`, `TRUNCATE`, `CREATE`, `ALTER`, `GRANT`, `REVOKE`, `MERGE`, `REPLACE`. Plus MySQL-specific blocks for `LOAD DATA`, `SELECT ... INTO OUTFILE`, `SELECT ... INTO DUMPFILE`.
2. **DBA recommendation** (documented, not enforced): grant the MySQL user used by DBBat read-only privileges (`GRANT SELECT ON db.* TO 'dbbat'@'%'`). This is the real protection; the regex is defense-in-depth.

## MySQL-Specific Blocked Operations

Always blocked, regardless of grant controls (even for non-read-only grants):

| Operation | Why |
|-----------|-----|
| `LOAD DATA INFILE` | Reads files from the MySQL server filesystem |
| `LOAD DATA LOCAL INFILE` | **Server can request the client to upload arbitrary local files** — major data exfiltration vector if upstream is compromised. The proxy refuses the protocol-level `LOCAL_INFILE_REQUEST` (response packet 0xFB) and blocks the SQL pattern. |
| `SELECT ... INTO OUTFILE` | Writes files to the MySQL server filesystem |
| `SELECT ... INTO DUMPFILE` | Same risk |
| `COM_BINLOG_DUMP` / `COM_BINLOG_DUMP_GTID` | Replication protocol — would let a client tail the binlog |
| `COM_REGISTER_SLAVE` | Replication protocol |
| `COM_SHUTDOWN` | Database shutdown |
| `COM_PROCESS_KILL` | Kills other sessions |
| `COM_DEBUG` | Server diagnostics, requires SUPER |

`COM_INIT_DB` (USE database) is allowed but logged — it changes session state we want visibility into.

## Database Model

No new MySQL-specific columns. The existing `databases` table fields are sufficient:
- `host`, `port`, `database_name`, `username`, `password_encrypted`, `ssl_mode` — all generic
- `protocol` — extended to accept `mysql`

The `port` column SQL default of `5432` (PG-centric) is dropped in this work; ports are validated as required at the API layer with protocol-aware suggested defaults (5432/1521/3306).

## Query Logging

Same model as PG/Oracle — every command is logged in the `queries` table with `sql_text`, `executed_at`, `duration_ms`, `rows_affected`, optional `error`. MySQL command coverage:

| MySQL command | Logged as |
|---------------|-----------|
| `COM_QUERY` | SQL text |
| `COM_STMT_PREPARE` | `PREPARE: <sql>` (logged once at prepare time) |
| `COM_STMT_EXECUTE` | The previously-prepared SQL with parameters in `parameters` JSONB |
| `COM_STMT_RESET` / `COM_STMT_CLOSE` | Not logged (housekeeping) |
| `COM_INIT_DB` | `USE <db>` synthetic SQL |
| `COM_PING` | Not logged (keepalive noise) |
| `COM_QUIT` | Not logged (handled by connection close) |

## Result Row Capture

**Phase 3 (v1):** Text-protocol result rows from `COM_QUERY` are captured up to `query_storage.max_result_rows` / `max_result_bytes` (same limits as PG). Rows are stored in the `query_rows` table as JSONB.

**v2 follow-up:** Binary-protocol rows from `COM_STMT_EXECUTE` require parsing each column according to its type code (24+ MySQL types). Deferred to a follow-up spec.

## Known Limitations (v1)

- **`caching_sha2_password` not supported** as the proxy-advertised auth plugin. Clients must accept `mysql_native_password` (default fallback for all major drivers).
- **No client-side TLS termination.** Proxy-to-client traffic is plaintext; deploy on a private network.
- **MariaDB untested.** The wire protocol is mostly compatible but MariaDB diverges on auth (`ed25519`), the `STMT_BULK_EXECUTE` command, and some type representations. Treat as best-effort.
- **Binary-protocol result row capture** (prepared statement EXECUTE results) not yet implemented — SQL text and parameters are logged, but rows aren't captured. Text protocol works fully.
- **Stored procedure multi-result-sets:** only the first result set is captured.
- **`COM_FIELD_LIST`** (deprecated since 5.7) is forwarded but not specially logged.

## Testing

Integration tests use the `testcontainers-go` MySQL module:

```go
container, err := mysql.RunContainer(ctx,
    testcontainers.WithImage("mysql:8.4"),
    mysql.WithDatabase("testdb"),
    mysql.WithUsername("test"),
    mysql.WithPassword("test"),
)
```

Tested clients (CI matrix):

| Client | Library | Status |
|--------|---------|--------|
| Go | go-sql-driver/mysql | full coverage |
| MySQL CLI | mysql 8.x | manual smoke test |
| Python | PyMySQL | manual smoke test |

For protocol debugging, set `DBB_LOG_LEVEL=debug` to see incoming MySQL commands and forwarded packets.
