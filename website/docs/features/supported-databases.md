---
sidebar_position: 0
---

# Supported Databases

DBBat ships with three independent listeners — one per wire protocol family. Enable only the engines you need by setting the matching `DBB_LISTEN_*` variable; an empty value disables that proxy.

| Engine | Protocol | Default proxy port | Env var | Status |
|--------|----------|--------------------|---------|--------|
| PostgreSQL | PostgreSQL wire (`pgx/v5`) | `:5434` | `DBB_LISTEN_PG` | First-class. Auth terminated at the proxy. MD5 and SCRAM clients work transparently. |
| Oracle | TNS / TTC | `:1522` | `DBB_LISTEN_ORA` | Hand-rolled TTC parser. End-to-end with `go-ora`; other clients reach AUTH but cannot yet execute queries (see notes below). |
| MySQL | MySQL wire (`go-mysql-org/go-mysql`) | `:3307` | `DBB_LISTEN_MYSQL` | `caching_sha2_password` (default), `mysql_clear_password`. TLS terminated at the proxy. `mysql_native_password` not supported. |
| MariaDB | MySQL wire (same listener) | `:3307` | `DBB_LISTEN_MYSQL` | Shares the MySQL listener. `STMT_BULK_EXECUTE` is refused — clients need batch-rewriting disabled. |

The same auth + grant + query-logging pipeline runs across all three protocols, so:

- **One user store** (Argon2id passwords, roles, optional Slack OAuth) authenticates against any engine.
- **One database catalogue** holds target connections; a `protocol` field marks the engine.
- **One grant model** applies the same controls (`read_only`, `block_copy`, `block_ddl`) and quotas regardless of upstream engine.
- **One query log** records every statement (`COM_QUERY`, `COM_STMT_EXECUTE`, PostgreSQL Simple/Extended Query, Oracle TTC Execute) in the same `queries` table.
- **One dump format** captures session traffic for any engine — see [the dump-format spec](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md).

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
- **Auth**: dbbat speaks O5LOGON to the client (per-user verifier loaded from an API key). The upstream session is re-authenticated using the database's stored credentials.
- **Query extraction**: SQL is parsed out of TTC `Execute` (function `0x03`, sub-op `0x5e`) packets; result rows are decoded for the first response (`func=0x10`) and continuation packets (`func=0x06`).
- **Number/Date decoding**: Oracle `NUMBER` and `DATE` formats are decoded for row capture.

### Tested clients

| Client | Status |
|--------|--------|
| `go-ora` | SQL + rows work end-to-end |
| Python `oracledb` (thin) | Authenticates; client rejects captured AUTH OK with `DPY-4035` |
| `ojdbc11` / DBeaver | SQL works, row capture partial |
| SQLcl 23c+ | Reaches AUTH; client rejects captured AUTH OK with `ORA-17401` |
| `sqlplus` (OCI) | Not yet supported (NS protocol not implemented; fails with `ORA-12630`) |

For now, use `go-ora` (or older thin-driver clients) end-to-end.

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

## Picking a Port

Default ports are chosen to avoid colliding with a co-located database server:

| Service | Default port |
|---------|--------------|
| PostgreSQL proxy | 5434 (PostgreSQL itself usually binds 5432) |
| Oracle proxy | 1522 (Oracle listener usually binds 1521) |
| MySQL/MariaDB proxy | 3307 (MySQL/MariaDB usually bind 3306) |
| REST API + web UI | 4200 |

Override any of them with the matching `DBB_LISTEN_*` environment variable.
