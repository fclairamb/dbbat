---
sidebar_position: 2
---

# Server Configuration

Target servers are configured through the REST API. Each entry maps a DBBat server name to a target (PostgreSQL, Oracle, MySQL, MariaDB, MongoDB, or Microsoft SQL Server), optionally reached through an SSH bastion.

:::note Endpoint rename
The endpoint is `/api/v1/servers` since v0.17.0 — it was `/api/v1/databases` before, and no alias is kept. The JSON response envelope is still `{"databases": [...]}`.
:::

## Creating a Server Configuration

### PostgreSQL

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "production",
    "description": "Production PostgreSQL",
    "protocol": "postgresql",
    "host": "prod-db.example.com",
    "port": 5432,
    "database_name": "myapp",
    "username": "app_user",
    "password": "secret",
    "ssl_mode": "require"
  }'
```

### Oracle

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "orcl",
    "description": "Oracle 19c",
    "protocol": "oracle",
    "host": "oracle.example.com",
    "port": 1521,
    "database_name": "ORCL",
    "oracle_service_name": "ORCL",
    "username": "scott",
    "password": "tiger",
    "ssl_mode": "disable"
  }'
```

`oracle_service_name` is what TNS clients use to route to this entry. It can match `database_name` or be different (e.g. for PDB names).

### MySQL / MariaDB

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "shop",
    "description": "Production MySQL",
    "protocol": "mysql",
    "host": "mysql.example.com",
    "port": 3306,
    "database_name": "shop",
    "username": "app_user",
    "password": "secret",
    "ssl_mode": "prefer"
  }'
```

For MariaDB, set `"protocol": "mariadb"`. Both share the same listener and proxy code path; the protocol field controls UI labelling and default-port hints.

### MongoDB

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "catalog",
    "description": "Production MongoDB",
    "protocol": "mongodb",
    "host": "mongo.example.com",
    "port": 27017,
    "database_name": "catalog",
    "username": "app_user",
    "password": "secret",
    "mongo_auth_source": "admin",
    "ssl_mode": "prefer"
  }'
```

`mongo_auth_source` is the upstream auth database DBBat authenticates against (defaults to `admin`, where root/service users are typically defined). Clients reach this entry by putting the DBBat database name in their connection's `authSource` (or using a `dbbatuser#catalog` username).

### Microsoft SQL Server

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "billing",
    "description": "Production SQL Server",
    "protocol": "mssql",
    "host": "mssql.example.com",
    "port": 1433,
    "database_name": "billing",
    "username": "app_user",
    "password": "secret",
    "ssl_mode": "prefer"
  }'
```

Clients name this entry in the LOGIN7 *Database* field, or select it from the login name as `User Id=alice#mssql-reporting` and put the real upstream database in *Database* — see [Naming the server from a client](#naming-the-server-from-a-client). Only SQL authentication is accepted; integrated (NTLM/Kerberos) and Entra ID logins are refused.

## Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `name` | string | DBBat server name — the **selector** clients use to pick this entry, never sent to the target. See [Naming the server from a client](#naming-the-server-from-a-client). | Yes |
| `protocol` | enum | `postgresql`, `oracle`, `mysql`, `mariadb`, `mongodb`, `mssql`, `ssh` | No (default: `postgresql`) |
| `host` | string | Target database host | Yes |
| `port` | integer | Target database port. Suggested defaults: 5432 / 1521 / 3306 / 27017 / 1433. | Yes |
| `database_name` | string | Target database name (or PDB name for Oracle) | Yes (PG/MySQL); recommended (Oracle) |
| `username` | string | Target database username | Yes |
| `password` | string | Target database password (encrypted at rest) | Yes |
| `ssl_mode` | string | SSL mode for the upstream connection | No (default: `prefer`) |
| `oracle_service_name` | string | Oracle SERVICE_NAME — used to route TNS connects | Recommended for Oracle |
| `mongo_auth_source` | string | MongoDB upstream auth database (defaults to `admin`) | No (MongoDB only) |
| `via_uid` | uuid | UID of an SSH bastion to tunnel through. `null` = dial the host directly. | No |
| `clear_via_uid` | bool | **PUT only.** `true` removes the tunnel and restores a direct dial. | No |
| `ssh_private_key` | string | PEM private key used to authenticate to the bastion. Write-only — never returned. | No (SSH only) |
| `ssh_passphrase` | string | Passphrase for an encrypted `ssh_private_key`. Write-only — never returned. | No (SSH only) |
| `ssh_known_host_key` | string | Read-only. The bastion's host key, pinned on the first successful connect (TOFU). | Never sent |
| `listable` | bool | Whether the server appears in the grant-request dropdown | No |
| `description` | string | Human-readable description | No |

:::note Duplicate names
Creating a server with a name that already exists returns `409 DUPLICATE_NAME`. The same applies to grant definitions and users.
:::

## SSH Tunnels

A server can be reached through an SSH bastion instead of being dialled directly. Bastions are managed under their own endpoint, `/api/v1/ssh-servers` (admin-only, `GET` + `POST`).

### Creating a bastion

```bash
curl -X POST http://localhost:4200/api/v1/ssh-servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "bastion-prod",
    "description": "Production jump host",
    "protocol": "ssh",
    "host": "bastion.example.com",
    "port": 22,
    "username": "dbbat",
    "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----\n"
  }'
```

Listing bastions returns a `{"servers": [...]}` envelope:

```bash
curl -H "Authorization: Bearer $DBBAT_API_KEY" http://localhost:4200/api/v1/ssh-servers
```

### Pointing a server at a bastion

Set `via_uid` to the bastion's UID:

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "private-pg",
    "protocol": "postgresql",
    "host": "10.0.3.14",
    "port": 5432,
    "database_name": "myapp",
    "username": "app_user",
    "password": "secret",
    "via_uid": "'"$BASTION_UID"'"
  }'
```

`host` and `port` are then resolved *from the bastion*, not from DBBat's own network. To remove the tunnel later, `PUT` with `"clear_via_uid": true`.

Tunnelling works for all five proxied protocols (PostgreSQL, Oracle, MySQL/MariaDB, MongoDB, SQL Server).

### Host-key pinning (TOFU)

The bastion's host key is accepted on the first successful connect and stored in `ssh_known_host_key` (trust on first use). Subsequent connections are verified against it and fail if the key changes.

:::warning
A changed host key means either a legitimate bastion rebuild or a man-in-the-middle. Clear `ssh_known_host_key` only once you have verified the new fingerprint out of band.
:::

### Connection pooling

Tunnelled connections go through a shared pooled dialer: one SSH connection to a bastion is reused across sessions instead of being re-established per client connection.

:::note
SSH bastion rows are excluded from the regular `/api/v1/servers` listing and from every grantable or connectable target context — they are not databases and cannot be proxied into. They appear only under `/api/v1/ssh-servers`.
:::

## SSL Modes

These follow the libpq convention and apply to the **upstream** connection:

- `disable` — No SSL
- `prefer` — Try SSL, fall back to plain (default)
- `require` — Require SSL, don't verify certificate
- `verify-ca` — Verify server certificate against CA
- `verify-full` — Verify certificate and hostname match

Client-side TLS for the proxy listeners is configured separately (e.g. `DBB_MYSQL_TLS_*` for the MySQL listener).

## Listing Servers

```bash
curl -H "Authorization: Bearer $DBBAT_API_KEY" http://localhost:4200/api/v1/servers
```

Response visibility depends on the caller's role:

| Role | What they see |
|------|---------------|
| Admin | Full details (host, port, database_name, username, ssl_mode, protocol, oracle_service_name) |
| Viewer | Limited (uid, name, description) |
| Connector | Only servers they have an active grant for (limited fields) |

Passwords and SSH private keys are **never** returned in any response. SSH bastions are not part of this listing — see [SSH Tunnels](#ssh-tunnels).

## Updating a Server

```bash
curl -X PUT http://localhost:4200/api/v1/servers/$DB_UID \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "description": "Updated description",
    "password": "new-secret"
  }'
```

Provide only the fields you want to update. Changing `password` re-encrypts the credential.

## Deleting a Server

```bash
curl -X DELETE http://localhost:4200/api/v1/servers/$DB_UID \
  -H "Authorization: Bearer $DBBAT_API_KEY"
```

Deleting a server configuration:

- Prevents new connections to that server
- Does not affect existing active connections
- Preserves all logged queries and connection history (for audit)

## Naming the server from a client

`name` is the **selector**: it is how a client says which DBBat entry it wants,
and it is never sent to the target. `database_name` is the real database on the
target, and it is what DBBat actually opens. The two are usually different — the
same upstream database is routinely registered twice, `demo_datalake_ro` and
`demo_datalake_rw`, both with `database_name: demo_datalake`.

For PostgreSQL, MySQL/MariaDB and SQL Server there are **three ways** to provide
the selector, tried in this order:

1. **Put the entry name in the database field.** The original form, and still
   the one that wins: `database=demo_datalake_ro`. Every connection string
   issued before this feature keeps working unchanged, and an exact entry-name
   match always beats an upstream database name — an entry can never be shadowed
   by somebody else's `database_name`.

2. **Put the entry name in the username, after a `#`.** The database field then
   carries the real upstream name:

   ```
   postgresql://alice%23demo_datalake_ro:$DBBAT_KEY@db.example.com:5432/demo_datalake
   ```

   `%23` is `#` inside a URL's userinfo; in a GUI where the user field is
   separate, type `alice#demo_datalake_ro` as-is. This is the form the UI hands
   out, because the username is the one field every driver and IDE preserves
   verbatim on every connection it opens. If the database field names something
   the entry does not expose, the connection is refused with a message saying
   which database that entry does expose — DBBat never silently redirects you to
   a different database than the one you asked for.

3. **Give the upstream database name alone.** DBBat matches it against the
   entries **you currently hold an active grant on**, on that protocol. Exactly
   one match connects. Several — the `_ro` / `_rw` twins — is refused, naming
   the candidates and telling you to pick one with `user#entry`. This rung can
   never reach an entry you were not already granted.

Oracle and MongoDB keep their own selectors: Oracle matches the connect
descriptor's `SERVICE_NAME` against `oracle_service_name` (falling back to
`name`), and MongoDB reads the SASL `authSource` first. MongoDB shares the
`user#entry` username parse — see [`docs/mongodb.md`](https://github.com/fclairamb/dbbat/blob/main/docs/mongodb.md).

## IDEs (DataGrip, DBeaver)

A JetBrains data source is a *server*, not a database. DataGrip lists
`pg_database`, reads the current database from `current_database()` — which
returns the **upstream** name, `demo_datalake`, not the DBBat entry name — and
then opens a dedicated connection per database with `database=<that name>`.
DBBat is a transparent proxy and does not rewrite catalog queries, so a data
source configured with only the entry name in the database field ends up
reconnecting to a name DBBat cannot resolve, and the tree stays empty.

Use form 2 above: put `alice#demo_datalake_ro` in **User**, and the real
`demo_datalake` in **Database**. The username survives every per-database
reconnect, so the entry stays selected. A reconnect to a database the entry does
not expose (`postgres`, `template1`) is refused with an explanatory message
rather than silently landing somewhere else.

DBeaver works the same way — user field, `#` suffix, real database name.

:::tip Older DBBat deployments
Against a DBBat that predates the `user#entry` selector, use JetBrains' **Single
database mode** (data source → Options). It exists for PgBouncer, whose pool
aliases have exactly these semantics, and it stops DataGrip from opening a
connection per database.
:::

## Connection Flow

When a user connects with `database=production`:

1. **PostgreSQL / MySQL / SQL Server**: DBBat resolves the entry through the three-rung ladder above — entry `name`, then the `user#entry` username selector, then the upstream `database_name` among the caller's active grants.
2. **Oracle**: DBBat matches the TNS connect descriptor's `SERVICE_NAME` against `oracle_service_name` (falls back to `name`).
3. **MongoDB**: DBBat resolves the entry from the SASL `authSource` (the DBBat database name), a `dbbatuser#name` username, or the user's single active MongoDB grant.
4. DBBat decrypts the stored credentials.
5. DBBat verifies the user has an active, non-revoked grant for this server.
6. If the entry has a `via_uid`, DBBat opens (or reuses, from the pooled dialer) an SSH connection to the bastion and dials `host:port` from there; otherwise it dials the host directly.
7. DBBat connects to the upstream using the stored credentials.
8. DBBat proxies all subsequent queries between client and target, logging everything.

## Storage-DSN Collision Warning

DBBat warns at startup if a configured target's `host:port/database_name` matches the DBBat storage DSN. Allowing developers to proxy *into* DBBat's own store is a privilege-escalation vector — keep them on separate databases (preferably separate clusters).
