---
model: opus
effort: high
---

# Add SSH tunnel support for upstream database connections (all protocols)

## Problem

Many target databases are only reachable through an SSH bastion (the DBeaver
"SSH" tab use case): the database listens on `localhost` of a remote host, and
clients must open an SSH tunnel first. DBBat currently dials targets directly
over TCP, so such databases cannot be proxied at all:

- PostgreSQL: `net.Dial` in `internal/proxy/postgresql/upstream.go:30`
- Oracle: `net.DialTimeout` in `internal/proxy/oracle/relay_preauth.go:498`
- MongoDB: `net.Dial` in `internal/proxy/mongodb/upstream.go:115`
- MySQL: `gomysqlclient.Connect` in `internal/proxy/mysql/upstream.go:53`

We want a database in DBBat to optionally declare "reach me through this SSH
host", transparently for the connecting end user.

### Reference test setup

A working DBeaver configuration exists to validate against (MySQL through SSH):

- SSH: host `195.154.39.225`, port `22`, user `www-data`, public-key auth with
  private key `/Users/florent/.ssh/id_rsa_keep` (no passphrase)
- Target (as seen from the SSH host): `localhost:3306`, database `opaline`,
  user `opaline`; the password is on Florent's machine in `/tmp/opaline.pass`
  (do not commit it anywhere)

## Design decision: bastion as a row in the (renamed) servers table

Options considered:

1. **Inline per-database** — an SSH section stored directly on the row (e.g.
   in the existing `protocol_data`-style jsonb pattern,
   `internal/store/models.go:196`). Rejected: a bastion typically fronts
   *several* databases, so key material gets duplicated and key rotation
   means editing every database.

2. **Separate `ssh_tunnels` table** with a nullable FK from `databases`.
   Clean separation, but adds a parallel table + store + API + UI surface
   that near-duplicates the databases one (host, port, user, encrypted
   secrets, CRUD).

3. **Bastion as a row in the same table** (chosen) — SSH bastions live in the
   existing table with `protocol = 'ssh'`, and every row gains a
   self-referencing `via_uid uuid NULL` FK: "dial this server through that
   one". A database and a bastion share the same storage shape
   (host/port/username/encrypted secret); the private key + passphrase fit
   the existing `ProtocolData` jsonb pattern (`SSHServerData` alongside
   `MongoDatabaseData`, `internal/store/models.go:207`). One CRUD surface,
   one encryption path, and multi-hop jump chains fall out for free
   (`via_uid` on an SSH row).

### Rename `databases` → `servers`

Since the table now holds SSH bastions too, "databases" is a misnomer. Rename
the table and model to **`servers`** / `Server`: an SSH bastion and a
database are both "a server dbbat knows how to reach" (host, port, username,
secret), with the existing `protocol` column as the discriminator
(`postgresql` | `oracle` | `mysql` | `mongodb` | `ssh`).

Rename scope:
- Migration: `ALTER TABLE databases RENAME TO servers` (+ FK/index renames);
  `database_name` and `ssl_mode` become nullable (meaningless for `ssh`).
- Go: `internal/store/databases.go` → `servers.go`, `Database` → `Server`,
  `DatabaseProtocolData` → `ServerProtocolData`; mechanical rename across
  store, api, proxies, cache.
- API: rename `/api/v1/databases` → `/api/v1/servers` (decided — pre-1.0, we
  control the clients; clean break, no `/databases` alias kept). OpenAPI spec
  and frontend API client follow.
- UI: the end-user-facing list can keep saying "Databases" where it lists
  targets (users think in databases); SSH servers appear in the admin
  server management, filtered out of grantable/target contexts.

### Cost accepted with this design

Everything downstream of the table currently assumes rows are grantable proxy
targets. The implementation MUST sweep every consumer and exclude
`protocol = 'ssh'` rows: grant definitions/requests, the listable API,
proxy connection-name lookup, UI database dropdowns, per-database stats.
Centralize this in one store-level query helper (e.g. a `targetsOnly` scope /
`WHERE protocol <> 'ssh'` applied in the store list/lookup functions) rather
than scattering conditions at call sites, and add a test that an SSH server
cannot be granted, listed as a database, or connected to by name.

## Proposal

1. **Migration + store**: rename `databases` → `servers`; add
   `via_uid uuid NULL REFERENCES servers`; relax `database_name`/`ssl_mode`
   to nullable; extend `ServerProtocolData` with
   `SSH *SSHServerData` (`private_key_encrypted`, `passphrase_encrypted`,
   `known_host_key`) — encrypted AES-256-GCM via `internal/crypto`, AAD-bound
   to the server UID like passwords. Add the store-level "targets only" scope
   described above. Validate: `via_uid` must point at a `protocol='ssh'` row;
   forbid cycles.

2. **Shared dialer**: in `internal/proxy/shared`, add
   `DialUpstream(ctx, srv *store.Server) (net.Conn, error)`: plain `net.Dial`
   when `via_uid` is null, else `sshClient.DialContext("tcp", host:port)`
   through a `golang.org/x/crypto/ssh` client (`x/crypto` is already a
   dependency, `go.mod:30`), recursing through `via_uid` chains for
   multi-hop. Maintain a pooled `ssh.Client` per bastion (mutex-guarded map
   keyed by server UID) so N sessions multiplex over one SSH connection;
   reconnect on failure.

3. **Host key verification**: store the bastion's public host key on first
   successful connect (TOFU) in `SSHServerData.known_host_key`, surface it in
   the API/UI, and reject on mismatch thereafter. Optionally allow an
   explicit fingerprint at creation.

4. **Wire the four proxies** to use the shared dialer:
   - postgresql/mongodb/oracle: replace the direct `net.Dial*` calls.
   - mysql: switch to `gomysqlclient.ConnectWithDialer` to inject the tunnel
     dialer.

5. **API + UI**: `/api/v1/servers` handles both kinds (secrets write-only,
   never returned); OpenAPI spec update; database create/edit form gains a
   "via SSH server" selector (pick existing `ssh` server or create one
   inline); admin UI to manage SSH servers. A "test tunnel" endpoint
   (mirroring DBeaver's "Test tunnel configuration") would be a nice
   follow-up.

6. **Validation**: end-to-end test against the reference setup above
   (MySQL `opaline` through `195.154.39.225`), plus unit tests with a fake
   SSH server (`x/crypto/ssh` test server) for the dialer, host-key logic,
   and the target-exclusion sweep (grants/listing/lookup refuse `ssh` rows).

### Open questions

- Keep-alives / idle teardown policy for pooled SSH clients.
- Should `ssh-agent` auth be supported? Probably not for a server-side proxy;
  key-in-store is the natural fit.
- Whether the UI keeps the "Databases" label for target lists vs a unified
  "Servers" page with protocol badges.

No GitHub issue filed yet — one should be created.

## Implementation Plan

Ordered sub-steps, each committed separately.

1. **Migration** (`internal/migrations/sql/20260716120000_databases_to_servers.{up,down}.sql`):
   `ALTER TABLE databases RENAME TO servers`; rename PK/index/FK constraint names
   referencing `databases`; add `via_uid uuid NULL REFERENCES servers(uid)`; drop NOT NULL
   on `database_name` and `ssl_mode`. Down reverses.

2. **Store rename sweep** (mechanical, word-boundary safe — `\bDatabase\b` etc.):
   - `internal/store/databases.go` → `servers.go` (+ `_test.go`), model `Database` → `Server`,
     `DatabaseProtocolData` → `ServerProtocolData`, `DatabaseUpdate` → `ServerUpdate`, store
     methods `*Database` → `*Server`, `ErrDatabaseNotFound` → `ErrServerNotFound`,
     `crypto.DatabaseAAD` → `crypto.ServerAAD`. Bun tag `table:databases` → `table:servers`.
   - Keep field/column names `DatabaseName`/`database_name`, `DatabaseID`/`database_id`,
     `MongoDatabaseData`, and API DTO type names (`CreateDatabaseRequest`, `DatabaseResponse`)
     unchanged — FK columns still legitimately reference database targets.
   - Add `ProtocolSSH = "ssh"`, `SSHServerData` (`private_key_encrypted`,
     `passphrase_encrypted`, `known_host_key`), `Server.SSHData()`, `Server.ViaUID *uuid.UUID`.
   - Encrypt/decrypt SSH secrets via `crypto.ServerAAD` (AES-256-GCM, AAD = server UID).
   - "targets-only" scope: `ListServers`/listable/lookup exclude `protocol='ssh'` where they
     feed grantable/connectable target contexts; add `ListSSHServers`/`ListAllServers` for admin.
   - Validate `via_uid` → must point at `protocol='ssh'` row; reject cycles (create/update).

3. **Shared dialer** (`internal/proxy/shared/dial.go`): `DialUpstream(ctx, srv, resolve, key)`
   → `net.DialTimeout` when `ViaUID==nil`, else dial through pooled `*ssh.Client` per bastion
   (mutex-guarded map keyed by UID), recursing `via_uid` chains. Host-key TOFU stored in
   `known_host_key`, verified/rejected on mismatch thereafter; persisted via a callback.

4. **Wire proxies**: postgresql `upstream.go`, oracle `relay_preauth.go`, mongodb `upstream.go`
   use `DialUpstream`; mysql `upstream.go` uses `gomysqlclient.ConnectWithDialer`.

5. **API + UI**: route `/api/v1/databases` → `/servers`; openapi.yml paths + SSH fields
   (write-only secrets, read-only `known_host_key`, `via_uid`); regenerate frontend schema;
   frontend API calls `/databases` → `/servers`; DB form gains "via SSH server" selector; SSH
   servers listed/managed in admin, excluded from target dropdowns.

6. **Tests**: fake in-process `x/crypto/ssh` server for dialer + TOFU + pooling; target-exclusion
   (ssh row not grantable/listable/lookup-able); via_uid validation + cycle rejection; encrypted
   secret round-trip. Real-host e2e only if egress to `195.154.39.225` exists.
