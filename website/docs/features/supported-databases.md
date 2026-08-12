---
sidebar_position: 0
---

# Supported Databases

DBBat ships with five independent listeners — one per wire protocol family. Enable only the engines you need by setting the matching `DBB_LISTEN_*` variable; an empty value disables that proxy.

| Engine | Protocol | Default proxy port | Env var | Status |
|--------|----------|--------------------|---------|--------|
| PostgreSQL | PostgreSQL wire (`pgx/v5`) | `:5433` | `DBB_LISTEN_PG` | First-class. Auth terminated at the proxy. MD5 and SCRAM clients work transparently. |
| Oracle | TNS / TTC | `:1522` | `DBB_LISTEN_ORA` | Hand-rolled TTC parser. End-to-end with `go-ora`; other clients reach AUTH but cannot yet execute queries (see notes below). |
| MySQL | MySQL wire (`go-mysql-org/go-mysql`) | `:3307` | `DBB_LISTEN_MYSQL` | `caching_sha2_password` (default), `mysql_clear_password`. TLS terminated at the proxy. `mysql_native_password` not supported. |
| MariaDB | MySQL wire (same listener) | `:3307` | `DBB_LISTEN_MYSQL` | Shares the MySQL listener. `STMT_BULK_EXECUTE` is refused — clients need batch-rewriting disabled. |
| MongoDB | MongoDB wire (`OP_MSG`, hand-rolled) | `:27018` | `DBB_LISTEN_MONGO` | Clients authenticate with `SCRAM-SHA-256` or `PLAIN`-over-TLS; upstream via `SCRAM-SHA-256`. Every command is classified, logged, and grant-checked. |
| Microsoft SQL Server | TDS (hand-rolled) | `:1434` | `DBB_LISTEN_MSSQL` | SQL authentication only (LOGIN7), TLS terminated at the proxy. `SQLBatch` and `RPC` are both enforced, not just logged. MARS and integrated/Entra ID auth are refused. |

**Any of these five can be reached through an SSH tunnel.** Point a server's `via_uid` at an SSH bastion server and its upstream connection is dialled through that bastion — see [SSH Tunnelling](#ssh-tunnelling) below.

The same auth + grant + query-logging pipeline runs across all five protocols, so:

- **One user store** (Argon2id passwords, roles, optional Slack OAuth) authenticates against any engine.
- **One server catalogue** holds target connections; a `protocol` field marks the engine.
- **One grant model** applies the same controls (`read_only`, `block_copy`, `block_ddl`) and quotas regardless of upstream engine.
- **One query log** records every statement (`COM_QUERY`, `COM_STMT_EXECUTE`, PostgreSQL Simple/Extended Query, Oracle TTC Execute, TDS `SQLBatch`/`RPC`) in the same `queries` table.
- **One capture format** records session traffic for any engine as standard pcapng — see [the capture-format spec](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md).

## PostgreSQL

The reference implementation. Both authentication and command-phase traffic are inspected.

- **Auth termination**: clients authenticate against the DBBat user store; DBBat re-authenticates upstream using the encrypted credentials in the database catalogue.
- **Read-only enforcement** is layered:
  1. Regex SQL inspection blocks `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `CREATE`, `ALTER`, `DROP`, `TRUNCATE`, `GRANT`, `REVOKE`, `COPY FROM`, `CALL`.
  2. The proxy issues `SET SESSION default_transaction_read_only = on` at session start.
  3. Attempts to disable read-only (`SET …`, `RESET`, `SET ROLE`, `SET SESSION AUTHORIZATION`) are blocked.
- **Result rows** are captured up to `query_storage.max_result_rows` / `max_result_bytes`.

## Oracle

Implemented as a hand-rolled TNS/TTC proxy in `internal/proxy/oracle`. See the full [protocol notes](https://github.com/fclairamb/dbbat/blob/main/docs/oracle.md) for wire-level details.

- **Connection routing** uses the `SERVICE_NAME` from the TNS connect descriptor — match it against either the database `name` or its `oracle_service_name`.
- **Auth**: dbbat speaks O5LOGON to the client — any of the user's API keys works as the password (per-user O5LOGON salts; verifiers loaded from the user's API keys). The upstream session is re-authenticated using the database's stored credentials.
- **Query extraction**: SQL is parsed out of TTC `Execute` (function `0x03`, sub-op `0x5e`) packets; result rows are decoded for the first response (`func=0x10`) and continuation packets (`func=0x06`).
- **Number/Date decoding**: Oracle `NUMBER` and `DATE` formats are decoded for row capture.

### Tested clients

Verified end-to-end (authenticate, query, and observability capture) against Oracle 23ai:

| Client | Status |
|--------|--------|
| `go-ora` | Works — SQL, rows and bind values end-to-end |
| Python `oracledb` (thin) | Works — verifier 18453 |
| SQLcl 26.1+ (ojdbc) | Works — classic O5LOGON, verifier 18453 |
| `sqlplus` (OCI instant client) | Works — via the wide (4-byte) TTC encoding, no `DISABLE_OOB` needed |
| `ojdbc11` / DBeaver | Connects, SQL logged; result-row capture partial |

Row capture is best-effort: the TTC binary layout varies across client versions, so some clients or query shapes capture partial rows. SQL text extraction is reliable across all of them.

## MySQL & MariaDB

Implemented in `internal/proxy/mysql` on top of `go-mysql-org/go-mysql`. See the full [MySQL notes](https://github.com/fclairamb/dbbat/blob/main/docs/mysql.md) for protocol-level details.

- **Auth termination**: `caching_sha2_password` is advertised by default. The fast-auth scramble is intentionally unused — every login takes the full-auth path so the cleartext password can be verified against the Argon2id hash (over TLS or RSA-OAEP).
- **TLS**: terminated at the proxy. `DBB_MYSQL_TLS_CERT_FILE` / `_KEY_FILE` provide the cert; if empty, a self-signed cert and RSA-2048 keypair are generated at startup.
- **`mysql_clear_password`**: accepted as a fallback for clients that explicitly pin to it.
- **`mysql_native_password`**: not supported (Argon2id is one-way; we cannot derive the SHA1 nested hash).
- **API key auth**: a "password" prefixed with `dbb_` is verified as an API key.

### Always-blocked operations (regardless of grant)

| Operation | Why |
|-----------|-----|
| `LOAD DATA INFILE` | Reads files from the MySQL server filesystem |
| `LOAD DATA LOCAL INFILE` | Server can request the client to upload arbitrary local files. Refused at SQL level *and* via `CLIENT_LOCAL_FILES` capability opt-out on the upstream connection. |
| `SELECT … INTO OUTFILE` / `INTO DUMPFILE` | Server-side filesystem write |
| `COM_BINLOG_DUMP` / `COM_REGISTER_SLAVE` | Replication protocol — would let a client tail the binlog |
| `COM_SHUTDOWN` / `COM_PROCESS_KILL` / `COM_DEBUG` | Privileged server operations |
| `STMT_BULK_EXECUTE` (MariaDB) | Not supported by go-mysql server side; refused |

### Logged commands

| MySQL command | Logged as |
|---------------|-----------|
| `COM_QUERY` | SQL text |
| `COM_STMT_PREPARE` | `PREPARE: <sql>` (logged once at prepare time) |
| `COM_STMT_EXECUTE` | The previously-prepared SQL with parameters in `parameters` JSONB |
| `COM_INIT_DB` | `USE <db>` synthetic SQL |
| `COM_PING`, `COM_QUIT`, `COM_STMT_RESET`, `COM_STMT_CLOSE` | Not logged (housekeeping / keepalive noise) |

### Tested clients

| Client | Library | Status |
|--------|---------|--------|
| Go | `go-sql-driver/mysql` | Full coverage in CI |
| MySQL CLI | `mysql` 8.x | Manual smoke test |
| Python | PyMySQL | Manual smoke test |
| MariaDB CLI | `mariadb` 10.x | Manual smoke test |

## MongoDB

Implemented as a hand-rolled MongoDB wire proxy in `internal/proxy/mongodb`. See the full [MongoDB notes](https://github.com/fclairamb/dbbat/blob/main/docs/mongodb.md) for protocol-level details.

- **Wire framing**: `OP_MSG` (2013) with kind-0 command bodies and kind-1 document sequences, plus the legacy `OP_QUERY`/`OP_REPLY` used for the first `hello`. BSON encode/decode via `go.mongodb.org/mongo-driver/v2/bson`. `OP_COMPRESSED` is rejected.
- **Auth termination**: two client mechanisms are terminated at the proxy:
  - **`SCRAM-SHA-256`** (driver default) — DBBat runs the server side of SCRAM against a stored verifier in `users.protocol_data.mongodb`, so the cleartext password never crosses the wire (TLS optional). Verifiers exist only for passwords set after this shipped.
  - **`PLAIN`-over-TLS** (`authMechanism=PLAIN`) — the driver sends the cleartext password (hence the TLS requirement), verified via the same Argon2id path as the other proxies. `dbb_` API keys work as the password.
- **Upstream**: DBBat authenticates to the target with `SCRAM-SHA-256` using the decrypted stored credentials. The upstream `authSource` defaults to `admin` and is overridable per server via `mongo_auth_source`.
- **Target-database resolution**: Mongo has no pre-auth database field, so the SASL `authSource` carries the DBBat database name (or a `dbbatuser#databasename` username, or the user's single active MongoDB grant).

## Microsoft SQL Server

Implemented as a hand-rolled TDS proxy in `internal/proxy/mssql`. See the full [SQL Server notes](https://github.com/fclairamb/dbbat/blob/main/docs/mssql.md) for protocol-level details.

- **Auth termination**: **SQL authentication only**. The username and password come out of LOGIN7 and go through the same Argon2id path as the other proxies; a password prefixed with `dbb_` is verified as an API key. The LOGIN7 *Database* field names the DBBat entry, not a database on the target. Refusals are returned as a proper TDS `ERROR` + `DONE` stream, so drivers surface the reason instead of a dropped socket.
- **TLS**: TDS encapsulates its TLS handshake inside TDS packets, so the proxy terminates it at the listener. `DBB_MSSQL_TLS_CERT_FILE` / `_KEY_FILE` provide the cert; if empty, a self-signed cert is generated at startup. `DBB_MSSQL_TLS_DISABLE=true` answers `ENCRYPT_NOT_SUP` and keeps the listener plaintext.
- **Query interception**: whole logical messages are classified before anything is forwarded. `SQLBatch` (text decoded from UCS-2LE) and `RPC` are both **enforced**, not merely logged — otherwise wrapping a write in `sp_executesql` would bypass `read_only`. Enforcement runs on the statement template; parameter values are captured for the query row.
- **`block_copy`** maps onto bulk copy: `BULK INSERT`, `INSERT BULK` and `OPENROWSET(BULK …)`, plus the `BulkLoadBCP` message that streams the rows.
- **Approval holds** work here like everywhere else, and a client `Attention` cancels a parked statement.

### Deliberately unsupported

| Feature | Why |
|---------|-----|
| **MARS** (`MultipleActiveResultSets=True`) | Refused in PRELOGIN — it multiplexes independent request streams over one connection, which the interception pipeline is not built for. |
| **Integrated authentication** (NTLM / Kerberos / SSPI) | Refused with a message telling the user to connect with a SQL login. |
| **Azure AD / Entra ID federated auth** | Declined in PRELOGIN and refused again if the LOGIN7 `FEATUREEXT` block still carries a `FEDAUTH` request. DBBat can neither mint nor validate a federated token. |
| **Changing a password through the proxy** | Not supported. |
| **TDS below 7.4** (pre SQL Server 2012) | The token forms DBBat emits are TDS 7.2+ wide forms; the floor is enforced at login. |

Stored-procedure bodies are not visible to DBBat — it sees `EXEC dbo.Something`, not what it does — so such a call fails closed under a restrictive grant.

## SSH Tunnelling

A target that is not directly reachable from DBBat can be dialled through an SSH bastion. This is transport-level and engine-agnostic: it works identically for PostgreSQL, Oracle, MySQL/MariaDB, MongoDB, and SQL Server.

- **Bastion entries** live in the same server catalogue, with `protocol: ssh`. They describe how to reach the bastion (host, port, SSH user, credentials) and nothing else — an SSH server is never itself a proxied target and can never be granted to a user. Managing them requires the `admin` role; they are also exposed under `/api/v1/ssh-servers`.
- **`via_uid`** on a database server points at such a bastion. When set, DBBat opens the upstream connection through the tunnel instead of dialling the target directly.
- **Host-key pinning (TOFU)**: the host key presented on the first successful connection is pinned, and every later connection must match it. The pinned key is readable as `ssh_known_host_key` so it can be checked against the bastion's real fingerprint.
- **Private keys and passphrases are write-only** — settable through the API, never returned by a read.
- **Pooled dialer**: one SSH transport connection per bastion is shared across many proxied sessions, each session getting its own forwarded channel. The database connections carried inside stay 1:1 with client sessions.

## Picking a Port

Default ports are chosen to avoid colliding with a co-located database server:

| Service | Default port |
|---------|--------------|
| PostgreSQL proxy | 5433 (PostgreSQL itself usually binds 5432) |
| Oracle proxy | 1522 (Oracle listener usually binds 1521) |
| MySQL/MariaDB proxy | 3307 (MySQL/MariaDB usually bind 3306) |
| MongoDB proxy | 27018 (MongoDB itself usually binds 27017) |
| SQL Server proxy | 1434 (SQL Server itself usually binds 1433) |
| REST API + web UI | 4200 |

Override any of them with the matching `DBB_LISTEN_*` environment variable.

`5433` is also the conventional port for pgbouncer. On a host that already runs
pgbouncer, set `DBB_LISTEN_PG` to a free port (e.g. `:5434`) to avoid a
collision.

`1434` looks like a collision with SQL Server and is not: the SQL Server Browser
service that owns 1434 is **UDP-only**, so 1434/tcp is free even on a host
already running SQL Server.
