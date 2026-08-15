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

Upstream connections **may** use TLS independently — the `servers.ssl_mode` column controls upstream encryption, honored regardless of the client-side TLS state. The policy lives in `upstream.PlanFor` (`internal/proxy/upstream/ssl.go`) and is executed by `upstream.ConnectMySQL` — the same connector the connectivity check runs, so a green check exercises the proxy's exact login:

| `ssl_mode` | Upstream behaviour |
|-----------|--------------------|
| `disable` | plaintext, never offers TLS |
| `allow` | plaintext first; retried with TLS only if the server refuses insecure transport |
| `prefer`, empty | TLS first; redials plaintext only if the server does not support TLS |
| `require` | TLS, **certificate not verified**; fails if the upstream doesn't offer TLS |
| `verify-ca`, `verify-full` | TLS with hostname + chain verification (`ServerName` = the server row's `host`, system root pool, TLS 1.2 floor); fails if the upstream doesn't offer TLS |

Three MySQL-specific notes versus the PG proxy:

- **Opportunistic TLS costs a redial.** go-mysql decides whether to encrypt from the handshake's `CLIENT_SSL` capability, and its option callback runs before that handshake is read, so `prefer` cannot be expressed as a single attempt. Only a *transport* failure may change the encryption of the retry — an authentication failure ends the chain, exactly as libpq's does.
- **`verify-ca` is treated as `verify-full`.** Both verify the hostname (Go's stdlib doesn't cleanly express CA-only verification), and there is no per-server CA bundle yet.
- **What actually happened is recorded.** Because `prefer` is a preference rather than a guarantee, each session stores the outcome in `connections.upstream_tls`; the row's `ssl_mode` alone cannot tell you whether a given session was encrypted.

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

### Switching database mid-session

A dbbat grant is issued on one server row, and `queries` carries no database
column of its own — only `connection_id`, with `connections.database_id` pinned
at auth. A session that moves to another database is therefore not merely
reaching data its grant never covered: every statement it runs afterwards is
*attributed* to the granted database. So a switch is refused outright, whatever
the grant says — a full-write grant on one database is not a grant on another,
which is the same reasoning behind Oracle's `ALTER SESSION SET CONTAINER` block.

A client can ask two ways, and both land on the same decision
(`handler.switchDatabase` in `internal/proxy/mysql/intercept.go`):

| Path | Treatment |
|------|-----------|
| `COM_INIT_DB` | refused unless it names the session's own database; logged either way |
| `USE <db>` as `COM_QUERY` text | same, and answered by the proxy — an allowed `USE` never reaches the upstream |
| `USE <db>` as `COM_STMT_PREPARE` | refused for every target: there is no OK packet to answer a prepare with, and MySQL does not accept `USE` as a preparable statement anyway |
| `PREPARE s FROM '<literal>'` whose literal is a `USE` | refused for every target, same reasoning — see below |

`USE <the granted database>` stays allowed under either name — the dbbat entry's
or the real one — because clients emit it routinely on connect. The comparison
is exact (MySQL's own case sensitivity for database names is
filesystem-dependent, so exact is the fail-closed direction).

The target is parsed off the comment-normalized scratch copy under the **MySQL**
dialect (`internal/proxy/shared/usedb.go`, `sqlcomments.go`), so `USE/**/otherdb`,
`USE # x⏎otherdb` and `USE/*!50100*/otherdb` are all read as the switches they
are. The match is anchored at the start of the statement, which is sound rather
than merely convenient: `USE` can only begin a statement, and the client leg does
not negotiate `CLIENT_MULTI_STATEMENTS`, so one `COM_QUERY` carries one
statement. Anything trailing the target other than a `;` is refused rather than
parsed.

### Dynamic SQL, and how far the checks reach into it

`PREPARE s FROM 'USE otherdb'` followed by `EXECUTE s` performs exactly the
switch above, one statement later, through text every check around it steps over
— `PREPARE` matches no write keyword, no DDL keyword and no blocked pattern. It
is the MySQL spelling of SQL Server's `EXEC('<literal>')`, MariaDB spells a
one-statement version of it `EXECUTE IMMEDIATE '<literal>'`, and all three are
closed the same way.

`shared.MySQLPreparedStatement` (`internal/proxy/shared/dynamicsql.go`, the one
static reader of dynamic SQL for all three protocols that have it) extracts the
literal — both quote characters, `''` doubling, backslash escapes, comments, case
folding — and the extracted text then goes through **the same checks the outer
statement got**: `shared.ValidateMySQLQuery` (grant controls *and* the
MySQL-specific blocked patterns) plus the `handler.switchDatabase` decision. A
second, laxer rule set for dynamic SQL would turn every control into a
suggestion.

| Form | Treatment |
|------|-----------|
| `PREPARE <name> FROM '<literal>'` / `"<literal>"` | statement text extracted, then classified and switch-checked like an ordinary statement |
| `EXECUTE IMMEDIATE '<literal>'` (MariaDB), with or without `USING …` | same |
| `EXECUTE <name>` whose `PREPARE` dbbat read and cleared | allowed |
| `EXECUTE <name>` whose `PREPARE` dbbat could **not** vouch for | refused under `read_only`/`block_ddl` only — see below |
| `PREPARE … FROM @sql`, `FROM CONCAT('USE ', @db)`, `EXECUTE IMMEDIATE @sql` | refused under `read_only`/`block_ddl` only, as `ErrDynamicSQLOpaque` |
| a nested `PREPARE`/`EXECUTE`, an unterminated literal, `FROM 'USE ' 'otherdb'` | **refused** whatever the grant says (`ErrPreparedTextNotCheckable` on the switch path, `shared.ErrDynamicSQLNotCheckable` on the control path) |

So `PREPARE s FROM 'DELETE FROM t'` is refused under `read_only` exactly as a
direct `DELETE` is, and `PREPARE s FROM 'DROP TABLE t'` under `block_ddl`. A
benign payload stays allowed: `PREPARE s FROM 'SELECT 1'` runs under a read-only
grant, because this is not a blanket refusal of dynamic SQL.

Unwrapping stops at one level, and stops loudly: a nested `PREPARE` is refused,
not unwrapped again, because stopping *silently* would leave a hole the exact
shape of the one this closes. So is a literal dbbat cannot read whole — MySQL
concatenates adjacent literals, so `FROM 'USE ' 'otherdb'` would leave dbbat
holding half a statement.

#### A payload dbbat cannot read: refused under `read_only`/`block_ddl` only

`PREPARE s FROM @sql` is assembled by the server from values dbbat never sees, so
nothing about what it runs is statically decidable. dbbat **refuses** it when the
active grant carries `read_only` **or** `block_ddl` — the two controls whose
meaning a run-time-built statement defeats outright. Fail closed: "dbbat cannot
tell what this runs" must not resolve to "allow" for a session that may not write
or may not change schema. The refusal names its own cause
(`shared.ErrDynamicSQLOpaque`), so an operator can tell it from an ordinary
blocked write and knows the fix is a grant without those two controls.

It is **not** refused for a grant carrying only `block_copy`, nor for a grant with
no controls at all: dynamic SQL defeats neither, so refusing there would be blast
radius bought for nothing — ORMs and migration tools build SQL at run time
constantly, and the traffic a wider refusal breaks is well-behaved traffic. The
rule is emphatically not "any control is set".

The `EXECUTE <name>` half follows from the same rule. It carries no statement text
at all, so it can only be answered from what the matching `PREPARE` said: the
session remembers which prepared names dbbat read and cleared
(`Session.checkedPrepares`), and under those two controls an `EXECUTE` of any
other name is refused. Checking the `PREPARE` and letting every `EXECUTE` through
would close nothing — a client would only have to build its text with
`PREPARE s FROM @sql` and then run it. Re-preparing a name replaces what it
stands for, so an opaque re-`PREPARE` clears the name rather than leaving a stale
"checked" flag.

What is still out of scope, stated plainly: a stored procedure or function whose
*body* builds SQL is opaque to all of this — dbbat sees the `CALL`, not the body.
A grant is scoped to a server row, and on MySQL that row's reach is whatever the
**upstream credentials** can see; if that boundary matters, constrain the login
dbbat connects with.

## Database Model

No new MySQL-specific columns. The existing `servers` table fields are sufficient:
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
- **A stored procedure or function body that builds SQL** is opaque: dbbat sees the `CALL`, never the body. See "Dynamic SQL, and how far the checks reach into it" for what *is* covered and for the `read_only`/`block_ddl` policy on a payload dbbat cannot read.

## Session Packet Dumps

When `DBB_DUMP_DIR` is set, the MySQL proxy writes a per-session `.pcapng` capture file containing the post-auth command-phase byte stream (matching the PG and Oracle proxies). The filename is the connection UID. Wiring is in `session.go: startDumpIfConfigured` — it swaps the underlying `net.Conn` on the live `packet.Conn` for a `dump.TapConn` after `recordConnection` runs, so the auth handshake itself is never captured. For TLS-upgraded connections the tap sees TLS records, which still preserves timing and packet boundaries.

## Testing

### Integration tests

`internal/proxy/mysql/integration_test.go` sits behind the `integration` build tag, so `make test` never runs it. CI runs `go vet -tags integration ./...`, which only proves it compiles — run it for real before trusting it:

```bash
# needs Docker; starts a MySQL upstream + a PostgreSQL container for dbbat's own store
go test -tags integration -timeout 40m ./internal/proxy/mysql/

# run the same matrix against another server version
MYSQL_TEST_IMAGE=mysql:9 MARIADB_TEST_IMAGE=mariadb:11 go test -tags integration -timeout 40m ./internal/proxy/mysql/
```

| Variable | Purpose |
|----------|---------|
| `MYSQL_TEST_IMAGE` | MySQL upstream image (default `mysql:8.4`) |
| `MARIADB_TEST_IMAGE` | MariaDB upstream image for `TestIntegration_MariaDB` (default `mariadb:10.11`) |

The suite covers the TLS handshake, query + prepared-statement (binary row) capture, `read_only` enforcement, `LOAD DATA LOCAL INFILE` blocking, session dumps, and the MariaDB flavour. Both default images have arm64 builds, so it runs unmodified on Apple Silicon (verified on 2026-07-21).

It uses the `testcontainers-go` MySQL module:

```go
container, err := tcmysql.Run(ctx, "mysql:8.4",
    tcmysql.WithDatabase("testdb"),
    tcmysql.WithUsername("root"),
    tcmysql.WithPassword("rootpw"),
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
