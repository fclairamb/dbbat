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

## Auth Strategy: `caching_sha2_password` (default), `mysql_clear_password` (legacy)

DBBat **terminates** authentication on the proxy side, the same way the PostgreSQL proxy does. The client's password is verified against the DBBat user store (Argon2id hashes) — never against upstream MySQL. DBBat then re-authenticates to upstream using the database's stored, encrypted credentials.

### Default plugin: `caching_sha2_password`

DBBat advertises `caching_sha2_password` as the server's default auth plugin during the initial handshake. This matches MySQL 8.0+ and works out of the box with every modern client.

Because dbbat stores passwords as Argon2id hashes — not MySQL's `SHA256(SHA256(PASSWORD))` — we cannot perform the fast-auth scramble validation. Every login takes the **full-auth** path:

1. Server advertises `caching_sha2_password`.
2. Client sends a scramble in `HandshakeResponse41`.
3. Server immediately replies with `AuthMoreData{0x04}` (full auth required).
4. Client sends the cleartext password — over TLS if the connection is secure, RSA-OAEP encrypted otherwise.
5. Server verifies the cleartext against the user's Argon2id hash.

The fast-auth cache is left empty by design. The performance cost is one extra round-trip per connection; the security gain is that we never store a plaintext-derived hash.

### Fallback plugin: `mysql_clear_password`

Clients that explicitly pin their auth plugin to `mysql_clear_password` (legacy drivers, some embedded systems) are accepted. The cleartext is verified against Argon2id the same way.

### `mysql_native_password` not supported

`mysql_native_password` requires the server to derive a SHA1-based hash from the stored password. Argon2id is one-way, so we cannot produce that hash; instead of advertising it and silently breaking on AuthSwitch, the proxy explicitly does not support it. All major drivers fall back to `caching_sha2_password` automatically.

### API key auth (proxy-side)

The PG proxy accepts DBBat API keys (prefix `dbb_`) as the password. The MySQL proxy does the same: when the proxy receives a "password" starting with the API key prefix, it verifies it as an API key instead of a user password.

## TLS Handling: Termination at the Proxy

DBBat **terminates TLS** at the proxy. When a client sends an `SSLRequest` packet during the handshake, the proxy upgrades the connection to TLS before reading credentials. Inside the TLS tunnel, the same handshake flow proceeds.

Configuration (env vars, all optional):

| Var | Description |
|-----|-------------|
| `DBB_MYSQL_TLS_DISABLE` | When `true`, the proxy refuses `SSLRequest` and stays plaintext-only. Default `false`. |
| `DBB_MYSQL_TLS_CERT_FILE` | Path to PEM-encoded server cert. |
| `DBB_MYSQL_TLS_KEY_FILE` | Path to PEM-encoded server key. Must be RSA for the non-TLS `caching_sha2` RSA-public-key path to work. |

If both cert/key paths are empty (and TLS isn't disabled), the proxy auto-generates a self-signed certificate and a fresh RSA-2048 keypair at startup. The same RSA key is reused for the `caching_sha2_password` public-key-retrieval path.

For production, supply a real certificate via the env vars. For development, the auto-generated cert is fine — clients will need `--ssl-mode=DISABLED` or the equivalent skip-verify option (or trust the cert).

Upstream connections **may** use TLS independently — the existing `databases.ssl_mode` column controls upstream encryption. The proxy honors the upstream SSL mode regardless of the client-side TLS state.

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
    │     (auth plugin: caching_sha2)                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  3. (optional) SSLRequest     │                                 │
    │──────────────────────────────>│ TLS upgrade                     │
    │<══════════════════════════════│                                 │
    │                               │                                 │
    │  4. HandshakeResponse41       │                                 │
    │     (user, db, scramble)      │                                 │
    │──────────────────────────────>│                                 │
    │                               │                                 │
    │  5. AuthMoreData{0x04}        │                                 │
    │<──────────────────────────────│                                 │
    │                               │                                 │
    │  6. cleartext password (TLS)  │                                 │
    │     OR RSA-encrypted (no TLS) │                                 │
    │──────────────────────────────>│ verify against Argon2id          │
    │                               │ (or API key)                    │
    │                               │                                 │
    │                               │  Look up user                   │
    │                               │  Look up database (by db_name)  │
    │                               │  Check active grant             │
    │                               │                                 │
    │                               │  7. Connect to upstream         │
    │                               │     (using stored creds)        │
    │                               │──────────────────────────────>│
    │                               │  8. Upstream handshake +        │
    │                               │     auth complete               │
    │                               │<─────────────────────────────>│
    │  9. OK packet                 │                                 │
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

## MariaDB Support

MariaDB is supported as a distinct protocol value (`mariadb`) alongside `mysql`. Both speak the MySQL wire protocol on the listener, so they share the same proxy code path; the distinction matters for:

- **UI labeling** — the database picker, badges, and placeholders show "MariaDB" explicitly.
- **Default port** — same as MySQL (3306) but surfaced separately for clarity.
- **Upstream auth negotiation** — the go-mysql client handles MariaDB's auth plugin negotiation transparently when connecting upstream. MariaDB 10.4+ defaults to `mysql_native_password` (not `caching_sha2_password`), so this just works.

### MariaDB-specific notes

- **`ed25519` auth plugin (MariaDB only):** the upstream client supports ed25519 for upstream connections if the MariaDB server is configured for it. The proxy itself never advertises ed25519 to clients.
- **`STMT_BULK_EXECUTE` (MariaDB-only command, 0x1A):** not supported. Currently falls into `HandleOtherCommand` and is refused. Clients that issue it (e.g., MariaDB Connector/J in batch-rewrite mode) need to disable batch rewriting.
- **Type representations:** mostly identical to MySQL. Edge cases (DECIMAL precision, JSON variants) inherit whatever go-mysql produces.

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
| `STMT_BULK_EXECUTE` (MariaDB) | Not supported by go-mysql server side; refused |

`COM_INIT_DB` (USE database) is allowed but logged — it changes session state we want visibility into.

## Database Model

No new MySQL-specific columns. The existing `databases` table fields are sufficient:
- `host`, `port`, `database_name`, `username`, `password_encrypted`, `ssl_mode` — all generic
- `protocol` — accepts `mysql` and `mariadb`

The `port` column SQL default of `5432` (PG-centric) was dropped; ports are validated as required at the API layer with protocol-aware suggested defaults (5432/1521/3306).

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

Result rows from both protocol paths are captured up to `query_storage.max_result_rows` / `max_result_bytes` (same limits as PG):

- **Text protocol** (`COM_QUERY`): rows arrive as `[]byte` per column and are encoded as UTF-8 strings or base64'd `$bytes`/`$type` markers for binary blobs.
- **Binary protocol** (`COM_STMT_EXECUTE`): go-mysql's high-level `Result.Resultset` decodes each column according to its type code before we see it, so the same `captureRows` path serializes both. Numeric columns become JSON numbers, JSON columns are parsed if valid, blobs are base64-marked, everything else is a string.

Rows are stored in the `query_rows` table as JSONB.

## Implementation Notes

### Reflection-based access to `Conn.salt`

The `caching_sha2_password` non-TLS RSA path needs the 20-byte challenge salt that go-mysql generates when it builds the initial handshake. The library exposes no public accessor, so `cachingsha2.go:readConnSalt` uses `reflect` + `unsafe` to read the unexported `salt` field on `*server.Conn`.

A self-test (`cachingsha2_test.go:TestReadConnSalt_FieldExists`) fails loudly if the field is renamed or removed in a future go-mysql release. This pins behavior to the dependency version in `go.mod`.

If the test fails after a `go.mod` upgrade: either pin go-mysql back, or extend the patch to expose `Salt()` upstream and remove the reflection.

## LOCAL INFILE Defense-in-Depth

`LOAD DATA LOCAL INFILE` is the MySQL feature that lets a server *ask* a connected client to upload an arbitrary local file. A compromised upstream server can issue this request mid-query against any client — including a proxy. Two layers prevent that:

1. **SQL regex** (shared with PG/Oracle) refuses the keyword in inbound client queries.
2. **Capability opt-out**: when the proxy connects upstream it explicitly clears `CLIENT_LOCAL_FILES` from the negotiated capabilities (`upstream.go: c.UnsetCapability(...)`). The upstream then never advertises the feature on this connection, so even a compromised server cannot request a LOCAL INFILE upload through the proxy.

## Known Limitations

- **Stored procedure multi-result-sets:** only the first result set is captured.
- **`COM_FIELD_LIST`** (deprecated since 5.7) is forwarded but not specially logged.
- **MariaDB `STMT_BULK_EXECUTE`** is refused (clients need to disable batch rewriting).
- **`mysql_native_password`** is intentionally not supported — all modern clients negotiate `caching_sha2_password` instead.
- **Session packet dumps** (`DBB_DUMP_DIR`) are not yet wired up for the MySQL proxy. PG and Oracle dump session traffic; MySQL does not.

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
| MariaDB CLI | mariadb 10.x | manual smoke test |

For protocol debugging, set `DBB_LOG_LEVEL=debug` to see incoming MySQL commands and forwarded packets.
