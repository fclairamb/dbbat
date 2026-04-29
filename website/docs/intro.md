---
sidebar_position: 1
---

# Introduction to DBBat

DBBat is a transparent database proxy designed for query observability, access control, and safety.

It gives temporary, audited access to production databases for support, debugging, and data analysis — without handing out raw credentials.

It speaks **PostgreSQL**, **Oracle**, and **MySQL/MariaDB** wire protocols, so any standard SQL client (psql, sqlplus, mysql, DBeaver, IntelliJ, pgAdmin, your application's ORM, …) can connect through DBBat without modification.

## Why DBBat?

Giving access to production databases can be dangerous. DBBat provides:
- **Query visibility**: every query is logged with its SQL text, parameters, duration, and rows affected
- **Result capture (optional)**: actual result rows can be stored alongside the query for replay/audit
- **Access control**: time-windowed grants with fine-grained controls for read-only, blocking COPY, blocking DDL
- **Audit trails**: append-only record of who did what — both inside the proxy and against the configuration API
- **Safety**: defense in depth against accidental writes, file-system-touching SQL, and protocol-level data exfiltration

DBBat addresses all these needs without requiring changes to your application code.

## Supported Databases

| Engine | Protocol | Default proxy port | Notes |
|--------|----------|--------------------|-------|
| PostgreSQL | PostgreSQL wire (`pgx/v5`) | `:5434` | First-class — auth terminated at the proxy, MD5/SCRAM clients work |
| Oracle | TNS / TTC | `:1522` | O5LOGON proxy auth, hand-rolled TTC parser. End-to-end with `go-ora`; other clients reach AUTH but do not yet execute queries |
| MySQL | MySQL wire (`go-mysql-org/go-mysql`) | `:3307` | `caching_sha2_password` (default), `mysql_clear_password`. TLS terminated at the proxy. `mysql_native_password` not supported |
| MariaDB | MySQL wire (same listener) | `:3307` | Same as MySQL — `STMT_BULK_EXECUTE` refused (clients need batch-rewriting off) |

Each engine has its own listener and is enabled independently via `DBB_LISTEN_PG` / `DBB_LISTEN_ORA` / `DBB_LISTEN_MYSQL`. Setting the variable to an empty string disables that proxy.

For protocol-level details, see:
- [Oracle proxy notes (TNS/TTC)](https://github.com/fclairamb/dbbat/blob/main/docs/oracle.md)
- [MySQL proxy notes](https://github.com/fclairamb/dbbat/blob/main/docs/mysql.md)
- [Dump file format](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md)

## Core Features

### Transparent Multi-Protocol Proxy

DBBat speaks each engine's native wire protocol. The same auth + grant + query-logging pipeline runs across all three.

```
psql / pg client     ─►  DBBat (auth + grant check + log) ─► PostgreSQL upstream
sqlplus / go-ora     ─►  DBBat (TNS service-name routing)  ─► Oracle upstream
mysql / mariadb cli  ─►  DBBat (caching_sha2_password)     ─► MySQL / MariaDB upstream
```

### User Management

- Users authenticate to DBBat with their own credentials (Argon2id-hashed passwords)
- Roles: `admin`, `viewer`, `connector` — combinable, additive
- Optional Slack OAuth for sign-in, with auto-provisioning configurable per default-role
- API keys (`dbb_…`) for programmatic access; intentionally cannot create or revoke other keys

### Database Configuration

- Store multiple target database connection details — one entry per database
- Credentials encrypted at rest with AES-256-GCM (AAD-bound to the database UID, so a stolen ciphertext cannot be transplanted)
- A `protocol` field marks each entry as `postgresql`, `oracle`, `mysql`, or `mariadb`
- For Oracle, an `oracle_service_name` is stored alongside the database name

### Connection & Query Tracking

- Track all connections with user, source IP, target database, timestamps, query count, and bytes transferred
- Log every query with SQL text, parameters, duration, rows affected, and any error
- Optional result-row capture, bounded by `max_result_rows` and `max_result_bytes`
- For MySQL, both text protocol (`COM_QUERY`) and binary protocol (`COM_STMT_EXECUTE`) are decoded and logged

### Access Control

- Grant time-windowed access (`starts_at`, `expires_at`) to specific databases
- Apply any combination of controls:
  - `read_only`: regex SQL inspection + PostgreSQL `default_transaction_read_only` + MySQL/MariaDB write blocks
  - `block_copy`: forbid `COPY` (PostgreSQL) and `LOAD DATA` / `SELECT … INTO OUTFILE` (MySQL)
  - `block_ddl`: forbid `CREATE`, `ALTER`, `DROP`, `TRUNCATE`
- Optional quotas: `max_query_counts`, `max_bytes_transferred`
- Automatic expiration or manual revocation, with audit log entries for every change

### Session Packet Dumps

Optional per-session binary dumps of the post-auth command stream, written as `.dbbat-dump` files. Same format across all protocols (see [the dump-format spec](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md)). Use `dbbat dump anonymise <input>` to strip session metadata before sharing a capture.

### REST API + Web UI

- Full OpenAPI 3.0 specification, served at `/api/openapi.yml`
- Swagger UI at `/api/docs`
- Embedded React frontend at `/app` for grant/user/database management and query browsing
- All API endpoints versioned under `/api/v1/`

## How It Works

Everything described here can be done via the REST API or the web UI.

1. **Admin creates a user**
2. **Admin configures a target database** (protocol, host, port, credentials, optional `oracle_service_name`)
3. **Admin grants the user access** to the database with a time window, controls, and optional quotas
4. **User connects** with `psql` / `sqlplus` / `mysql` / any client, using their DBBat credentials (or an API key)
5. **DBBat authenticates** the user, checks for a valid grant, and connects to the upstream using the stored encrypted credentials
6. **DBBat proxies** all queries to the target database, logging everything

## Security

- **User passwords**: Argon2id (configurable preset and parameters)
- **Database credentials**: AES-256-GCM, AAD-bound to the database UID
- **API keys**: encrypted blobs, scoped restrictions (no key-management via key)
- **Encryption key**: from `DBB_KEY` (base64) or `DBB_KEYFILE`; auto-generated at `~/.dbbat/key` on first start if neither is set
- **Default admin**: created on first startup (username: `admin`, password: `admin`) — must be changed before login

## Try the Demo

Experience DBBat without any setup. Our demo instance is available at:

**[demo.dbbat.com](https://demo.dbbat.com)**

- Login: `admin` / `admin`
- Data resets periodically
- Explore all features freely

## Next Steps

- [Install DBBat](/docs/installation/docker) using Docker
- [Configure](/docs/configuration) your environment
- Learn about [Access Control](/docs/features/access-control)
