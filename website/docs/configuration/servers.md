---
sidebar_position: 2
---

# Server Configuration

Target servers are configured through the REST API. Each entry maps a DBBat server name to a target (PostgreSQL, Oracle, MySQL, MariaDB, or MongoDB), optionally reached through an SSH bastion.

:::note Endpoint rename
The endpoint is `/api/v1/servers` since v0.17.0 — it was `/api/v1/databases` before, and no alias is kept. The JSON response envelope is still `{"databases": [...]}`.
:::

## Creating a Server Configuration

### PostgreSQL

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $TOKEN" \
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
  -H "Authorization: Bearer $TOKEN" \
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
  -H "Authorization: Bearer $TOKEN" \
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
  -H "Authorization: Bearer $TOKEN" \
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

## Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `name` | string | DBBat server name (used by clients in their connection string) | Yes |
| `protocol` | enum | `postgresql`, `oracle`, `mysql`, `mariadb`, `mongodb`, `ssh` | No (default: `postgresql`) |
| `host` | string | Target database host | Yes |
| `port` | integer | Target database port. Suggested defaults: 5432 / 1521 / 3306 / 27017. | Yes |
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
  -H "Authorization: Bearer $TOKEN" \
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
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/ssh-servers
```

### Pointing a server at a bastion

Set `via_uid` to the bastion's UID:

```bash
curl -X POST http://localhost:4200/api/v1/servers \
  -H "Authorization: Bearer $TOKEN" \
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

Tunnelling works for all four proxied protocols (PostgreSQL, Oracle, MySQL/MariaDB, MongoDB).

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
curl -H "Authorization: Bearer $TOKEN" http://localhost:4200/api/v1/servers
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
  -H "Authorization: Bearer $TOKEN" \
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
  -H "Authorization: Bearer $TOKEN"
```

Deleting a server configuration:

- Prevents new connections to that server
- Does not affect existing active connections
- Preserves all logged queries and connection history (for audit)

## Connection Flow

When a user connects with `database=production`:

1. **PostgreSQL / MySQL**: DBBat looks up the entry by `name` (the database name in the client's connection string).
2. **Oracle**: DBBat matches the TNS connect descriptor's `SERVICE_NAME` against `oracle_service_name` (falls back to `name`).
3. **MongoDB**: DBBat resolves the entry from the SASL `authSource` (the DBBat database name), a `dbbatuser#name` username, or the user's single active MongoDB grant.
4. DBBat decrypts the stored credentials.
5. DBBat verifies the user has an active, non-revoked grant for this server.
6. If the entry has a `via_uid`, DBBat opens (or reuses, from the pooled dialer) an SSH connection to the bastion and dials `host:port` from there; otherwise it dials the host directly.
7. DBBat connects to the upstream using the stored credentials.
8. DBBat proxies all subsequent queries between client and target, logging everything.

## Storage-DSN Collision Warning

DBBat warns at startup if a configured target's `host:port/database_name` matches the DBBat storage DSN. Allowing developers to proxy *into* DBBat's own store is a privilege-escalation vector — keep them on separate databases (preferably separate clusters).
