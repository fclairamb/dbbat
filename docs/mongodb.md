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

## Auth Strategy: `PLAIN` over TLS (client) → SCRAM-SHA-256 (upstream)

MongoDB drivers default to SCRAM-SHA-256/SHA-1, which cannot be verified against
dbbat's Argon2id password hashes. So the client authenticates to dbbat with SASL
`PLAIN` (the driver's `authMechanism=PLAIN`), which puts the cleartext password
on the wire — hence the TLS requirement — and dbbat verifies it via the same
Argon2id path as the other proxies.

- `dbb_` API keys work as the password for free (`isAPIKey` → `VerifyAPIKey` +
  ownership check).
- `PLAIN` is refused on non-TLS connections unless TLS is disabled entirely for
  the listener (`DBB_MONGO_TLS_DISABLE`).

Upstream, dbbat decrypts the stored credentials (AES-256-GCM, AAD-bound to the
database UID) and authenticates with SCRAM-SHA-256 as a client, SASLprep-ing the
password. The upstream `authSource` is `admin` (where MongoDB service/root users
are typically defined, e.g. `MONGO_INITDB_ROOT_USERNAME`).

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
`topologyVersion`, or `compression`. Clients must pass `directConnection=true`.

`maxWireVersion` is pinned to `21` (MongoDB 7.0): modern enough for current
drivers (Go driver v2 requires ≥ 7), low enough not to invite features we don't
proxy. Supported upstream servers: 6.0 / 7.0 / 8.0.

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
other database. `listDatabases` is denied by default (cluster-wide disclosure).

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

## Session Packet Dumps

When `DBB_DUMP_DIR` is set, the post-auth (plaintext, TLS-terminated) framed
traffic is captured per session using `dump.ProtocolMongo`. Dumps are pruned per
`DBB_DUMP_RETENTION`.

## Known Limitations

- **Transactions and retryable writes** are refused *client-side* by drivers
  because dbbat presents as a standalone (both require replica-set/sharded
  topology). Acceptable for an observability proxy.
- **PLAIN requires TLS.** Clients must pass
  `authMechanism=PLAIN&authSource=<dbbat-db-name>&directConnection=true`. The
  `authSource` doubles as the dbbat database selector.
- **Upstream `authSource` is fixed to `admin`.** Targets whose proxy user lives
  in a different auth database are not yet supported (a later enhancement can
  add a per-database column).
- **No wire compression** (`OP_COMPRESSED` refused), **no streaming/awaitable
  hello**, **no exhaust cursors**.
- `listDatabases` is denied (later: result filtering).
- **Stored SCRAM verifiers** in `users.protocol_data` (letting clients keep
  default SCRAM instead of PLAIN) and **`loadBalanced` mode** are future work.

## Testing

`internal/proxy/mongodb/wire_test.go` covers the framing round-trips.
`internal/proxy/mongodb/integration_test.go` (build tag `integration`) dials a
`mongo:7` testcontainer **through** the proxy with the official Go driver
(`authMechanism=PLAIN`, `directConnection=true`, TLS with InsecureSkipVerify),
exercising auth (password + API key), the monitoring-connection path,
find/insert round-trips, read-only and block_ddl enforcement, `$db` violations,
wrong-password failures, session dumps, and mid-session quota/revocation
teardown.
