---
sidebar_position: 1
---

# Introduction to DBBat

DBBat is a transparent database proxy designed for query observability, access control, and safety.

It gives developers and AI agents temporary, audited access to production databases for support, debugging, and data analysis — without handing out raw credentials.

It speaks the **PostgreSQL**, **Oracle**, **MySQL/MariaDB**, **MongoDB**, and **Microsoft SQL Server** wire protocols, so any standard database client (psql, sqlplus, mysql, mongosh, sqlcmd, DBeaver, IntelliJ, pgAdmin, your application's ORM or driver, an AI agent's database tooling, …) can connect through DBBat without modification.

## Why DBBat?

Giving access to production databases can be dangerous. DBBat provides:
- **Query visibility**: every query is logged with its SQL text, parameters, duration, and rows affected
- **Result capture (optional)**: actual result rows can be stored alongside the query for replay/audit
- **Access control**: time-windowed grants with fine-grained controls for read-only, blocking COPY, blocking DDL
- **Audit trails**: a record of who did what — both inside the proxy and against the configuration API — [HMAC-chained](/docs/features/audit-chain) so that editing, deleting or reordering an entry is detectable, even by whoever runs DBBat's own storage database
- **Safety**: defense in depth against accidental writes, file-system-touching SQL, and protocol-level data exfiltration
- **Agent-ready**: AI agents connect with the same unmodified drivers and get the same guardrails — time-boxed grants, read-only enforcement, quotas, and approval holds on risky statements

DBBat addresses all these needs without requiring changes to your application code.

If your compliance team needs to know what evidence all of this produces, [Compliance](/docs/compliance) maps each capability to the SOC 2, ISO/IEC 27001:2022 and PCI DSS v4.x controls auditors ask about — and is equally explicit about what DBBat does not do.

## Supported Databases

| Engine | Protocol | Default proxy port | Notes |
|--------|----------|--------------------|-------|
| PostgreSQL | PostgreSQL wire (`pgx/v5`) | `:5433` | First-class — auth terminated at the proxy, MD5/SCRAM clients work |
| Oracle | TNS / TTC | `:1522` | O5LOGON proxy auth, hand-rolled TTC parser. End-to-end with `go-ora`; other clients reach AUTH but do not yet execute queries |
| MySQL | MySQL wire (`go-mysql-org/go-mysql`) | `:3307` | `caching_sha2_password` (default), `mysql_clear_password`. TLS terminated at the proxy. `mysql_native_password` not supported |
| MariaDB | MySQL wire (same listener) | `:3307` | Same as MySQL — `STMT_BULK_EXECUTE` refused (clients need batch-rewriting off) |
| MongoDB | MongoDB wire (`OP_MSG`, hand-rolled) | `:27018` | Clients authenticate with `SCRAM-SHA-256` or `PLAIN`-over-TLS; upstream via `SCRAM-SHA-256`. `authSource` carries the DBBat database name |
| Microsoft SQL Server | TDS (hand-rolled) | `:1434` | SQL authentication only (LOGIN7), TLS terminated at the proxy. `SQLBatch` and `RPC` are both enforced. MARS and integrated/Entra ID auth are refused |

Each engine has its own listener and is enabled independently via `DBB_LISTEN_PG` / `DBB_LISTEN_ORA` / `DBB_LISTEN_MYSQL` / `DBB_LISTEN_MONGO` / `DBB_LISTEN_MSSQL`. Setting the variable to an empty string disables that proxy.

**Any of the five can be reached through an SSH tunnel.** Register an SSH bastion as a server with `protocol: ssh`, then set the target server's `via_uid` to it — DBBat dials the upstream through the bastion. Bastion host keys are pinned trust-on-first-use.

For protocol-level details, see:
- [Oracle proxy notes (TNS/TTC)](https://github.com/fclairamb/dbbat/blob/main/docs/oracle.md)
- [MySQL proxy notes](https://github.com/fclairamb/dbbat/blob/main/docs/mysql.md)
- [MongoDB proxy notes](https://github.com/fclairamb/dbbat/blob/main/docs/mongodb.md)
- [SQL Server proxy notes (TDS)](https://github.com/fclairamb/dbbat/blob/main/docs/mssql.md)
- [Session capture format](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md)

## Core Features

### Transparent Multi-Protocol Proxy

DBBat speaks each engine's native wire protocol. The same auth + grant + query-logging pipeline runs across all five.

```
psql / pg client     ─►  DBBat (auth + grant check + log) ─► PostgreSQL upstream
sqlplus / go-ora     ─►  DBBat (TNS service-name routing)  ─► Oracle upstream
mysql / mariadb cli  ─►  DBBat (caching_sha2_password)     ─► MySQL / MariaDB upstream
mongosh / driver     ─►  DBBat (SCRAM-SHA-256 / PLAIN-TLS) ─► MongoDB upstream
sqlcmd / driver      ─►  DBBat (TDS PRELOGIN + LOGIN7)     ─► SQL Server upstream
```

### User Management

- Users authenticate to DBBat with their own credentials (Argon2id-hashed passwords)
- Roles: `admin`, `viewer`, `connector` — combinable, additive
- Optional Slack OAuth for sign-in, with auto-provisioning configurable per default-role
- API keys (`dbb_…`) for programmatic access; intentionally cannot create or revoke other keys

### Server Configuration

- Store multiple target connection details — one server entry per database
- Credentials encrypted at rest with AES-256-GCM (AAD-bound to the server UID, so a stolen ciphertext cannot be transplanted)
- A `protocol` field marks each entry as `postgresql`, `oracle`, `mysql`, `mariadb`, `mongodb`, or `ssh`
- An `ssh` entry is a bastion definition rather than a proxied target: another server's `via_uid` points at it, and that server's upstream connection is then dialled through the tunnel
- For Oracle, an `oracle_service_name` is stored alongside the server name; for MongoDB, an optional `mongo_auth_source` selects the upstream auth database (defaults to `admin`)

### Connection & Query Tracking

- Track all connections with user, source IP, target database, timestamps, query count, and bytes transferred
- Log every query with SQL text, parameters, duration, rows affected, and any error
- Optional result-row capture, bounded by `max_result_rows` and `max_result_bytes`
- For MySQL, both text protocol (`COM_QUERY`) and binary protocol (`COM_STMT_EXECUTE`) are decoded and logged

### Access Control

- Grant time-windowed access (`starts_at`, `expires_at`) to specific servers
- Apply any combination of controls:
  - `read_only`: regex SQL inspection + PostgreSQL `default_transaction_read_only` + MySQL/MariaDB write blocks
  - `block_copy`: forbid `COPY` (PostgreSQL) and `LOAD DATA` / `SELECT … INTO OUTFILE` (MySQL)
  - `block_ddl`: forbid `CREATE`, `ALTER`, `DROP`, `TRUNCATE`
- Optional quotas: `max_query_counts`, `max_bytes_transferred`
- **Grant definitions**: reusable templates describing what a user may request, so access does not have to be hand-crafted each time
- **Self-service grant requests**: users request access against a definition instead of pinging an admin
- **Auto-approve**: a grant definition can be flagged `auto_approve`, so matching requests are approved instantly. A justification is still required, the resulting grant is audit-tagged `via: auto_approve`, and the Slack notification is sent without Approve/Deny buttons
- Automatic expiration or manual revocation, with audit log entries for every change
- Revocation is immediate: it blocks further queries **and tears down sessions already live** under the grant, across all protocols

### Session Packet Dumps

Optional per-session captures of the post-auth command stream, written as tcpdump-compatible `.pcapng` files that Wireshark dissects natively. Same format across all protocols (see [the capture-format spec](https://github.com/fclairamb/dbbat/blob/main/docs/dump-format.md)). Use `dbbat dump anonymise <input>` to strip session metadata and the synthesized addresses before sharing a capture.

### REST API + Web UI

- Full OpenAPI 3.0 specification, served at `/api/openapi.yml`
- Swagger UI at `/api/docs`
- Embedded React frontend at `/app` for grant/user/server management and query browsing — servers live at `/servers`
- Connection detail pages, reachable from the query list and from a query's breadcrumb
- All API endpoints versioned under `/api/v1/`

## How It Works

Everything described here can be done via the REST API or the web UI.

1. **Admin creates a user**
2. **Admin configures a target server** (protocol, host, port, credentials, optional `oracle_service_name`)
3. *(Optional)* **Admin adds an SSH bastion server** (`protocol: ssh`) and sets the target's `via_uid` to it, so the upstream connection is dialled through the tunnel
4. **Admin defines a grant definition** (controls, quotas, duration) and assigns it to the user on that server — or the user requests it themselves against the same definition. A grant is always an instance of a definition; it carries no access rules of its own
5. **User connects** with `psql` / `sqlplus` / `mysql` / `mongosh` / any client, using their DBBat credentials (or an API key)
6. **DBBat authenticates** the user, checks for a valid grant, and connects to the upstream using the stored encrypted credentials
7. **DBBat proxies** all queries to the target database, logging everything

## Security

- **User passwords**: Argon2id (configurable preset and parameters)
- **Database credentials**: AES-256-GCM, AAD-bound to the database UID
- **API keys**: Argon2id-hashed like passwords — only the 8-character prefix is stored in clear, for lookup — with scoped restrictions (no key-management via key)
- **Audit log and query history**: [HMAC-chained](/docs/features/audit-chain) with a key derived from `DBB_KEY` and never stored in the database; `dbbat audit verify` walks the chain
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
- Hand your auditors the [Compliance](/docs/compliance) mapping
