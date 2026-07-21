# MongoDB Proxy — Protocol Notes

DBBat proxies the MongoDB wire protocol so that every command a client runs is
authenticated with dbbat credentials, grant-checked, classified, logged, and
quota-enforced — the same pipeline as the PostgreSQL, Oracle and MySQL proxies.

The listener defaults to `:27018` (`DBB_LISTEN_MONGO`; empty disables it).

## Library Choice

No Go library offers a MongoDB *server* handshake, so the wire framing is
hand-rolled (like the Oracle proxy) following an explicit contract. BSON
document encode/decode uses `go.mongodb.org/mongo-driver/v2/bson`; the
client-side SCRAM toward the upstream uses `github.com/xdg-go/scram`.

The framing lives in `internal/proxy/mongodb/wire.go`: the 16-byte message
header, `OP_MSG` (2013) with kind-0 command bodies and kind-1 document
sequences, the legacy `OP_QUERY` (2004) / `OP_REPLY` (1) used for the first
`hello`, and rejection of `OP_COMPRESSED` (2012). A trailing CRC-32C checksum
is tolerated on parse and never emitted on our own replies.

## Auth Strategy: `PLAIN`-over-TLS or `SCRAM-SHA-256` (client) → SCRAM-SHA-256 (upstream)

MongoDB drivers default to SCRAM-SHA-256/SHA-1, which cannot be verified against
dbbat's Argon2id password hashes. dbbat therefore terminates one of two client
mechanisms:

- **SASL `PLAIN`** (`authMechanism=PLAIN`): the driver puts the cleartext
  password on the wire — hence the TLS requirement — and dbbat verifies it via
  the same Argon2id path as the other proxies. `dbb_` API keys work as the
  password for free (`isAPIKey` → `VerifyAPIKey` + ownership check). PLAIN is
  refused on non-TLS connections unless TLS is disabled entirely for the listener
  (`DBB_MONGO_TLS_DISABLE`).
- **`SCRAM-SHA-256`** (the driver default, no explicit `authMechanism`): dbbat
  runs the server side of SCRAM against a **stored verifier** in
  `users.protocol_data.mongodb` (`internal/proxy/mongodb/scram_server.go`). The
  verifier — salt, iteration count, and the encrypted StoredKey/ServerKey — is
  derived and persisted on every password set (`SetUserMongoVerifier`), so the
  cleartext password never crosses the wire and TLS is optional. Verifiers only
  exist for passwords set after this shipped; `hello`'s `saslSupportedMechs`
  advertises `SCRAM-SHA-256` only for users that have one, and PLAIN stays the
  fallback for everyone else.

Upstream, dbbat decrypts the stored credentials (AES-256-GCM, AAD-bound to the
database UID) and authenticates with SCRAM-SHA-256 as a client, SASLprep-ing the
password. The upstream `authSource` defaults to `admin` (where MongoDB
service/root users are typically defined, e.g. `MONGO_INITDB_ROOT_USERNAME`) and
is configurable per database via the `mongo_auth_source` API field (stored in
the generic `databases.protocol_data` jsonb column) for targets whose proxy
user lives in a different auth database.

### Target-database resolution

Mongo has no pre-auth database field, so the SASL `authSource` carries the dbbat
database name. Resolution order (`internal/proxy/mongodb/auth.go`):

1. `saslStart.$db` not in `{$external, admin}` → that database name.
2. Username of the form `dbbatuser#databasename`.
3. The user's single active MongoDB grant.
4. Otherwise auth fails (code 18) with a message explaining the
   `authSource=<dbname>` convention.

The connection-string builder emits `authSource=<dbbat database name>`.

## TLS Handling: Termination at the Proxy

MongoDB TLS is implicit-from-byte-0 (no STARTTLS dance). The session peeks the
first client byte (`0x16` = TLS handshake) to support both TLS and plaintext on
one listener, then terminates TLS at the proxy. Certificate resolution mirrors
the MySQL/PG proxies:

- `DBB_MONGO_TLS_DISABLE=true` — plaintext listener, no TLS.
- `DBB_MONGO_TLS_CERT_FILE` + `DBB_MONGO_TLS_KEY_FILE` — load from disk.
- both empty (default) — an in-memory self-signed cert is generated at startup
  (fine for development; provide a real cert in production).

## Topology Discovery (why we present as a standalone)

Drivers discover servers from the `hello` reply and will connect *directly* to
any hosts it advertises, bypassing the proxy. dbbat therefore **synthesizes** the
client-facing `hello` from static values (it cannot forward-and-rewrite the
upstream's reply, because `hello` arrives before dbbat knows the target). The
reply presents a **standalone** server: no `hosts`, `setName`, `primary`, `me`,
or `topologyVersion`. Clients pass `directConnection=true`, **or** connect with
`loadBalanced=true` (MongoDB 5.0+) — dbbat then answers `hello` with a stable
per-process `serviceId` so the driver pins cursors/transactions to the
connection and never tries to discover or dial the real host. `loadBalanced` is
the cleaner topology story for an L4 proxy than `directConnection`.

`maxWireVersion` is pinned to `21` (MongoDB 7.0): modern enough for current
drivers (Go driver v2 requires ≥ 7), low enough not to invite features we don't
proxy. Supported upstream servers: 6.0 / 7.0 / 8.0.

### Wire compression

If the client offers `zlib` in its `hello` `compression` list, dbbat echoes it
and then transparently inflates inbound `OP_COMPRESSED` (opcode 2012) frames
before classification, forwarding the decompressed frame upstream (dbbat never
negotiates compression upstream). Replies mirror the client — dbbat compresses an
`OP_MSG`/`OP_REPLY` reply only once the client has itself sent a compressed
frame, which keeps the first `hello` reply plain and guarantees the client will
decompress ours. Only `zlib` (stdlib) is negotiated; `snappy`/`zstd` are
declined.

### Monitoring connections

Drivers open separate connections that send `hello`/`ping` periodically and
never authenticate. The proxy answers these indefinitely and **does not** create
a `store.Connection` or dial the upstream for them — the connection record is
created only on a successful `saslStart`, mirroring the MySQL proxy.

## Connection Flow

```
Client → DBBat (peek 0x16 → TLS, PLAIN auth, grant check) → Target MongoDB (SCRAM-SHA-256)
```

Example connection string (as produced by the dbbat UI / API):

```
mongodb://user:dbb_xxx@db.company.com:27018/mydb?authMechanism=PLAIN&authSource=<dbbat-db-name>&tls=true&directConnection=true
```

mongosh smoke test:

```
mongosh "mongodb://user:pass@localhost:27018/mydb" \
  --authenticationMechanism PLAIN --authenticationDatabase <dbbat-db-name> \
  --tls --tlsAllowInvalidCertificates
```

## Command Classification & Enforcement

The command name is the first key of the `OP_MSG` kind-0 body. Classification
(`shared.ValidateMongoCommand`) is driven by `grant.IsReadOnly()` /
`ShouldBlockDDL()`:

- **Read**: `find`, `aggregate` (write when the pipeline contains `$out`/`$merge`),
  `count`, `distinct`, `getMore`, `killCursors`, `listCollections`, `listIndexes`,
  `explain`, `dbStats`, `collStats`.
- **Write**: `insert`, `update`, `delete`, `findAndModify`, `bulkWrite`.
- **DDL**: `create`, `drop`, `dropDatabase`, `createIndexes`, `dropIndexes`,
  `collMod`, `renameCollection`, `convertToCapped`.
- **Diagnostics (always allowed post-auth)**: `hello`/`isMaster`/`ismaster`,
  `ping`, `buildInfo`, `whatsmyuri`, `connectionStatus`, `getParameter`,
  `getLog`, `hostInfo`, `atlasVersion`, `endSessions`, `saslStart`,
  `saslContinue`.
- **Always blocked**: `createUser`, `updateUser`, `dropUser`,
  `dropAllUsersFromDatabase`, `grantRolesToUser`, `revokeRolesFromUser`,
  `createRole`, `updateRole`, `dropRole`, `shutdown`, `replSetReconfig`,
  `replSetStepDown`, `setParameter`, `setFeatureCompatibilityVersion`, `eval`,
  `fsync`, `compact`.
- **Unknown commands: default-deny** with `Unauthorized` (13) and a logged
  command name — the allowlist is extended from real logs, not guessed.

### `$db` enforcement

Allow the configured database (its dbbat `Name` or upstream `DatabaseName`);
allow `admin` only for the diagnostics set; deny `local`, `config`, and any
other database. `listDatabases` is **allowed but filtered**: the reply's
`databases` array is rewritten to just the grant's target database (with
`totalSize`/`totalSizeMb` recomputed), so no cluster-wide database list leaks.

`block_copy` has no MongoDB equivalent (PostgreSQL COPY-specific).

Blocked commands surface an `Unauthorized` (13) error document carrying the
dbbat reason. Fire-and-forget (`w:0`, `moreToCome`) writes that are blocked are
dropped silently and logged — the client is not listening for a reply.

## Query Logging & Result Capture

- `Query.SQLText` = command name + canonical Extended JSON of the kind-0 body
  (truncated). `Parameters` may carry `lsid`/`txnNumber`.
- `RowsAffected` from the reply's `nModified`/`n`.
- Result rows from `cursor.firstBatch` / `cursor.nextBatch`, each BSON document
  re-encoded as Extended JSON into `query_rows`. `getMore` replies are attributed
  to the originating query because dbbat is a 1:1 connection proxy and the reply's
  `responseTo` correlates to the client `requestID`.
- **Cursor lineage:** when a `find`/`aggregate`/`listCollections`/`listIndexes`
  reply opens a server cursor, its `Parameters` are stamped with `cursor_id=<id>`;
  each subsequent `getMore` for that cursor is stamped with the same `cursor_id`
  plus `cursor_origin=<command> <namespace>`, so a whole paged result set reads as
  one logical query. The link is dropped when the cursor drains (`cursor.id == 0`).

## Session Packet Dumps

When `DBB_DUMP_DIR` is set, the post-auth (plaintext, TLS-terminated) framed
traffic is captured per session using `dump.ProtocolMongo`. Dumps are pruned per
`DBB_DUMP_RETENTION`.

## Known Limitations

- **Transactions and retryable writes** against a standalone upstream are refused
  *client-side* by drivers (both require replica-set/sharded topology).
  `loadBalanced=true` pins cursors/transactions to the connection, but the
  upstream must itself be a replica set / sharded cluster for multi-document
  transactions to run. Acceptable for an observability proxy.
- **PLAIN requires TLS.** When authenticating with `authMechanism=PLAIN`, clients
  must connect over TLS (`authSource=<dbbat-db-name>`, which doubles as the dbbat
  database selector). Clients with a stored SCRAM-SHA-256 verifier can use the
  driver default instead and skip the TLS requirement.
- **No `snappy`/`zstd` compression** (only `zlib`), **no streaming/awaitable
  hello**, **no exhaust cursors**.

## Testing

`internal/proxy/mongodb/wire_test.go` covers the framing round-trips (including
`OP_COMPRESSED` compress/decompress); `filter_test.go` and `lineage_test.go`
cover listDatabases filtering and cursor lineage as unit tests.

`internal/proxy/mongodb/integration_test.go` (build tag `integration`) dials a
`mongo:7` testcontainer **through** the proxy with the official Go driver,
exercising auth (PLAIN password + API key, and driver-default SCRAM-SHA-256 via a
stored verifier), the monitoring-connection path, find/insert round-trips,
unacknowledged (`w:0`/`moreToCome`) writes, `zlib` wire compression,
`listDatabases` filtering, `getMore` cursor lineage, read-only and block_ddl
enforcement, `$db` violations, wrong-password failures, session dumps, and
mid-session quota/revocation teardown.

The tagged suite is excluded from `make test`. CI only runs
`go vet -tags integration ./...`, which proves it compiles, not that it works —
run it for real with Docker available:

```bash
go test -tags integration -timeout 40m ./internal/proxy/mongodb/
```

Set `MONGO_TEST_IMAGE=mongo:8` (or `mongo:6`) to run the same matrix against
another server version:

```
MONGO_TEST_IMAGE=mongo:8 go test -tags integration ./internal/proxy/mongodb/...
```

| Variable | Purpose |
|----------|---------|
| `MONGO_TEST_IMAGE` | Upstream MongoDB image (default `mongo:7`) |

The default `mongo:7` and the `postgres:15-alpine` store container both have
arm64 builds, so the suite runs unmodified on Apple Silicon (verified on
2026-07-21).
