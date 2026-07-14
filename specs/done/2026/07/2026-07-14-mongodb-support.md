---
model: opus
effort: xhigh
---
# MongoDB protocol support

## Goal

Add MongoDB as a fourth proxied protocol: clients connect to a dbbat listener
(default `:27018`, env `DBB_LISTEN_MONGO`) with their dbbat credentials or a
`dbb_` API key, dbbat authenticates to the target MongoDB with stored
credentials, and every command is grant-checked, classified
(read/write/DDL), logged, and quota-enforced — same pipeline as
PostgreSQL/Oracle/MySQL.

No GitHub issue filed yet — one should be created.

## Why

DBBat's value proposition is protocol-agnostic observability and access
control. MongoDB is the most common non-SQL production datastore; teams that
front their PG/MySQL/Oracle access through dbbat currently have zero
audit/grant story for Mongo.

## Implementation

Template: clone `internal/proxy/mysql/` (structure) but hand-roll the wire
framing like Oracle, since no Go library offers a MongoDB *server*
handshake. The wire protocol is far simpler than TNS/TTC; the complete
implementation contract is in the **Wire protocol contract** section below —
phase 1 must follow it exactly, it is not optional background.

`go.mongodb.org/mongo-driver/v2` is already in `go.sum` (transitive via
testcontainers): use its `bson` package for message payloads, and
`github.com/xdg-go/scram` (+ `xdg-go/stringprep` for SASLprep, also
transitive — the official driver's own SCRAM implementation) for client-side
SCRAM toward the target.

Authoritative references (pin these, do not code wire details from memory):
- https://www.mongodb.com/docs/manual/reference/mongodb-wire-protocol/
- https://github.com/mongodb/specifications — `source/message/`,
  `source/mongodb-handshake/handshake.md`, `source/auth/auth.md`

### Hard problem 1 — client-side auth (SCRAM vs Argon2id)

Mongo drivers default to SCRAM-SHA-256/SHA-1, which cannot be verified
against our Argon2id hashes. Two options, both with in-repo precedent:

1. **Force `PLAIN` over TLS** (recommended first step). Drivers support
   `authMechanism=PLAIN` (their LDAP path — but it is plain SASL PLAIN on
   the wire, so it works against our server side regardless of "Enterprise"
   labeling). The client sends the cleartext password; we verify via
   `cache.AuthCache` / Argon2id exactly like the PG cleartext request
   (`internal/proxy/postgresql/auth.go:80`) and MySQL caching_sha2 full-auth
   (`internal/proxy/mysql/cachingsha2.go`). Also makes `dbb_` API keys work
   as passwords for free (`isAPIKey` + `VerifyAPIKey`, same as both existing
   proxies). Require TLS before accepting PLAIN (we terminate TLS with the
   same self-signed-or-provided cert pattern, `DBB_MONGO_TLS_*`).
2. **Stored SCRAM verifiers** (later, optional): persist per-user
   SCRAM-SHA-256 verifiers in `users.protocol_data` jsonb under a `"mongodb"`
   key — exact same hook as Oracle O5LOGON verifiers
   (`internal/store/models.go:50`, migration
   `20260712210000_users_protocol_data`). Lets clients keep default SCRAM,
   but verifiers only exist for passwords set after the feature ships.

Target side: decrypt creds (`Database.DecryptPassword`,
`internal/store/databases.go:331`), dial raw TCP, run SCRAM-SHA-256 as a
client (see contract §5).

### Hard problem 2 — topology discovery

Drivers discover servers from the `hello` response and will connect
*directly* to any hosts it advertises, bypassing the proxy. dbbat must
**synthesize** the client-facing `hello` reply from static values — it
cannot forward-and-rewrite the upstream's reply, because `hello` arrives
*before* authentication, i.e. before dbbat knows which target database the
session is for. Present as a **standalone** server (no `hosts`, `setName`,
`primary`, `me`, no `topologyVersion`), and document `directConnection=true`
in client connection strings. Exact reply template in contract §3.

Cursor correctness is free: dbbat is a 1:1 connection proxy, so
`getMore`/`killCursors` naturally reach the same upstream connection.
`loadBalanced=true` mode (MongoDB 5.0+, designed for L4 proxies) is a
possible later enhancement, not phase 1.

---

### Wire protocol contract (phase-1 implementation contract)

#### §1 Message framing

All integers little-endian. Every message starts with a 16-byte header:

```
int32 messageLength   // total, including this header
int32 requestID       // sender-chosen
int32 responseTo      // requestID being answered (0 in requests)
int32 opCode
```

Opcodes the proxy must handle:

| opCode | Name          | Direction / use |
|--------|---------------|-----------------|
| 2013   | OP_MSG        | everything (client↔server), MongoDB ≥ 3.6 |
| 2004   | OP_QUERY      | legacy client request — **still sent by most drivers for the first `hello`** |
| 1      | OP_REPLY      | legacy server reply to OP_QUERY |
| 2012   | OP_COMPRESSED | never negotiated by us — if received anyway, close the connection with an error log |

**OP_MSG** body:

```
uint32 flagBits
sections (one or more):
  kind byte 0x00: one BSON document (the command body; first key = command
                  name; contains "$db")
  kind byte 0x01: int32 size, cstring identifier, then consecutive BSON docs
                  (a command array field hoisted out, e.g. insert's
                  "documents", update's "updates")
[uint32 CRC-32C]  // only if flagBits bit 0 set
```

Flag bits:
- bit 0 `checksumPresent` (0x1): 4-byte CRC-32C (Castagnoli) trails the
  message. Tolerate on parse (strip/ignore), never set on our own replies.
  Verbatim relay (§6) keeps client checksums intact.
- bit 1 `moreToCome` (0x2): sender expects **no reply**. Clients set it for
  unacknowledged `w:0` writes → do not reply (see §6). Never set it in our
  replies (we never stream).
- bit 16 `exhaustAllowed` (0x10000): client permits streamed replies.
  Ignore; we never stream (no `topologyVersion` advertised → drivers poll).

Replies are OP_MSG with a single kind-0 section; `responseTo` = the
request's `requestID`.

**OP_QUERY** (parse only, for the first handshake message):

```
int32  flags
cstring fullCollectionName   // will be "admin.$cmd"
int32  numberToSkip
int32  numberToReturn
BSON   query                 // {isMaster: 1, helloOk: true, client: {...}, ...}
[BSON  returnFieldsSelector] // may be absent
```

**OP_REPLY** (emit only, answering an OP_QUERY hello):

```
int32 responseFlags   // set 8 (AwaitCapable); no error bits
int64 cursorID        // 0
int32 startingFrom    // 0
int32 numberReturned  // 1
BSON  document        // the hello response document (§3)
```

A client that handshakes via OP_QUERY switches to OP_MSG for everything
after (guaranteed by advertising maxWireVersion ≥ 6, §4).

#### §2 Connection lifecycle

Per-connection state machine:

```
TCP accept → [TLS upgrade] → PRE-AUTH → AUTHENTICATED → relay loop
                                └── or stays PRE-AUTH forever (monitoring conn)
```

- **Monitoring connections are normal.** Drivers (including the Go driver
  and mongosh) open separate connections that send `hello` periodically
  (default every 10 s in polling mode) and **never authenticate and never
  send commands**. The proxy must answer their hellos indefinitely, must not
  require auth on them, and must **not** create a `store.Connection` record
  or dial the upstream for them. Create the Connection record and connect
  upstream only on successful `saslStart` (mirrors MySQL: record after
  auth). This is the #1 "works in unit tests, fails with a real driver"
  trap.
- PRE-AUTH command allowlist (anything else → `Unauthorized` error, §7):
  `hello`, `isMaster`/`ismaster` (accept both spellings), `ping`,
  `buildInfo`, `saslStart`, `saslContinue`, `endSessions`.
- TLS: same termination pattern as MySQL/PG (`tls.go`), env
  `DBB_MONGO_TLS_DISABLE` / `DBB_MONGO_TLS_CERT_FILE` / `DBB_MONGO_TLS_KEY_FILE`.
  Mongo TLS is implicit-from-byte-0 when the client enables it (no STARTTLS
  dance): peek the first byte (0x16 = TLS handshake) to support both TLS and
  plaintext on one listener, or terminate unconditionally when certs are
  configured. PLAIN auth must be refused on non-TLS connections unless
  explicitly allowed by config.

#### §3 The synthesized `hello` reply

Answer `hello`/`isMaster` locally with exactly this document (BSON), whether
it arrived as OP_QUERY (reply OP_REPLY) or OP_MSG (reply OP_MSG):

```
{
  isWritablePrimary: true,        // when the command was "hello"
  // ismaster: true,              // instead, when the command was isMaster/ismaster
  helloOk: true,                  // echo only if the client sent helloOk: true
  maxBsonObjectSize: 16777216,
  maxMessageSizeBytes: 48000000,
  maxWriteBatchSize: 100000,
  localTime: <BSON UTC datetime, now>,
  logicalSessionTimeoutMinutes: 30,
  connectionId: <int, per-listener counter>,
  minWireVersion: 0,
  maxWireVersion: 21,             // see §4
  readOnly: false,
  ok: 1.0
}
```

Deliberately **omitted** (each omission is load-bearing):
- `hosts`, `setName`, `primary`, `me`, `secondary`, `arbiterOnly`, `msg` —
  present as standalone so the driver never dials the real host.
- `topologyVersion` — its absence forces polling `hello` instead of the
  awaitable/streaming (`maxAwaitTimeMS` + exhaust) protocol. Never implement
  streaming in phase 1.
- `compression` — never echo the client's list; absence = no OP_COMPRESSED.
- `speculativeAuthenticate` — clients may embed
  `speculativeAuthenticate: {saslStart: 1, mechanism: "SCRAM-SHA-256", ...}`
  in the hello. **Omit the field in the reply**; per the handshake spec the
  driver then falls back to an explicit `saslStart`, which is where our
  PLAIN-only negotiation happens. Do not error on its presence.

If the hello contains `saslSupportedMechs: "<db>.<user>"`, add
`saslSupportedMechs: ["PLAIN"]` to the reply.

#### §4 Wire version pinning

| maxWireVersion | Server |
|----------------|--------|
| 6 | 3.6 (OP_MSG minimum) |
| 7 | 4.0 |
| 8 | 4.2 |
| 9 | 4.4 |
| 13 | 5.0 |
| 17 | 6.0 |
| 21 | 7.0 |
| 25 | 8.0 |

Advertise a pinned constant `maxWireVersion = 21` (7.0): modern enough for
current drivers (Go driver v2 requires ≥ 7), low enough not to invite
features we don't proxy. Supported upstream servers: 6.0/7.0/8.0. Presenting
as standalone means drivers disable retryable writes and refuse transactions
client-side — acceptable, list under Known limitations.

#### §5 Auth exchanges

**Client → dbbat (SASL PLAIN):**

Request (OP_MSG body):
```
{saslStart: 1, mechanism: "PLAIN",
 payload: BinData(0, base64("\x00" + username + "\x00" + password)),
 autoAuthorize: 1, $db: "$external"}
```
- Payload is RFC 4616: `[authzid] \0 authcid \0 password`; drivers send an
  empty authzid. Parse defensively (split on NUL, take last two fields).
- Accept `$db` of `$external` or `admin` (drivers default PLAIN's
  authSource to `$external`; users may override).
- Verify password via `authCache.VerifyPassword` (Argon2id) or, if it has
  the `dbb_` prefix, the `isAPIKey` → `store.VerifyAPIKey` + ownership path
  (clone MySQL `auth.go:101-146`).
- Success reply: `{conversationId: 1, done: true, payload: BinData(0, ""), ok: 1.0}`.
- Failure reply: `{ok: 0.0, errmsg: "Authentication failed.", code: 18,
  codeName: "AuthenticationFailed"}` — then close after a short delay.
- On success: resolve database + active grant
  (`store.GetActiveGrant`), register revocation, build `shared.LimitGuard`
  + `Watch()`, create the `store.Connection`, dial upstream.
- **Target-database resolution** (pinned): Mongo has no pre-auth database
  field like PG's startup param or MySQL's handshake schema, but the
  `saslStart` command's `$db` *is* the client's `authSource` — so we make
  the authSource carry the dbbat database name. Resolution order:
  1. `saslStart.$db` not in {`$external`, `admin`} → that's the database
     name (connection strings use `authSource=<dbname>`; our
     connection-string builder emits this).
  2. Otherwise, if the username contains `#`, split as
     `dbbatuser#databasename`.
  3. Otherwise, if the user has exactly one active grant, use it.
  4. Otherwise fail auth with code 18 and an errmsg explaining the
     `authSource=<dbname>` convention.

**dbbat → upstream (SCRAM-SHA-256):**

1. TCP dial (TLS per `db.SSLMode`, reuse MySQL `upstream.go` mapping).
2. Send our own `hello` as OP_MSG with `client` metadata; set
   `client.application.name` via `shared.BuildUpstreamName` (branding
   parity with the other proxies).
3. SCRAM via `xdg-go/scram` (SHA-256, SASLprep the password with
   `xdg-go/stringprep`):
   - `{saslStart: 1, mechanism: "SCRAM-SHA-256", payload: <client-first>,
      options: {skipEmptyExchange: true}, $db: <authSource>}`
   - loop `{saslContinue: 1, conversationId, payload}` until reply has
     `done: true`.
   - `authSource`: new nullable column or `protocol_data` entry on the
     database row; default `admin`.
4. Fall back to SCRAM-SHA-1 only if the target rejects SHA-256 (pre-4.0 —
   out of declared support range, so optional).

#### §6 Relay rules (post-auth)

- Phase 1 relays **verbatim bytes** both ways after auth: client requestIDs
  pass through untouched, so upstream `responseTo` values already correlate
  — do not re-frame. (Our locally-answered hello/auth replies used our own
  requestIDs pre-splice; no conflict.)
- Phase 2 parses each client OP_MSG before forwarding (splice point:
  identical to MySQL — swap the raw copy loop for a parse→validate→forward
  loop). Kind-1 document sequences matter here: an `insert`'s documents
  arrive in a kind-1 section, not the body — classification reads the
  kind-0 body (command name, `$db`), logging may serialize both.
- `moreToCome` requests (`w:0` unacknowledged writes): forward without
  expecting or producing a reply. If validation blocks one, **drop it
  silently and log** — the client is not listening for a reply, and this
  matches server behavior for failed unacknowledged writes.
- Dumps: splice `dump.NewTapConn` on the client conn right after auth
  (pattern: `mysql/session.go:267`); add `dump.ProtocolMongo` constant.

#### §7 Error reply shape

Locally-generated errors are OP_MSG kind-0:
`{ok: 0.0, errmsg: "<human text>", code: <int>, codeName: "<name>"}`.
Codes used: `18 AuthenticationFailed`, `13 Unauthorized` (blocked/denied
commands, read-only violations, `$db` violations). Include the dbbat reason
in `errmsg` (e.g. "dbbat: grant is read-only") — this is what the user sees
in mongosh.

---

### Command classification & enforcement (phase 2)

Command name = first key of the OP_MSG kind-0 body. Add a
`ValidateMongoCommand` alongside `ValidateMySQLQuery`
(`internal/proxy/shared/validation.go`) operating on command names, driven
by `grant.IsReadOnly()` / `ShouldBlockDDL()`:

- **Read**: `find`, `aggregate` (write if pipeline contains `$out` or
  `$merge` — inspect the pipeline array), `count`, `distinct`, `getMore`,
  `killCursors`, `listCollections`, `listIndexes`, `explain`, `dbStats`,
  `collStats`.
- **Write**: `insert`, `update`, `delete`, `findAndModify`, `bulkWrite`.
- **DDL**: `create`, `drop`, `dropDatabase`, `createIndexes`, `dropIndexes`,
  `collMod`, `renameCollection`, `convertToCapped`.
- **Diagnostics (always allowed post-auth)** — the set a real mongosh
  session emits on connect: `hello`/`isMaster`/`ismaster`, `ping`,
  `buildInfo`, `whatsmyuri`, `connectionStatus`, `getParameter`, `getLog`,
  `hostInfo`, `atlasVersion` (pass through; upstream answers CommandNotFound
  and that's fine), `endSessions`, `saslStart`, `saslContinue`.
- **Always blocked** (mirror MySQL's refused admin commands):
  `createUser`, `updateUser`, `dropUser`, `dropAllUsersFromDatabase`,
  `grantRolesToUser`, `revokeRolesFromUser`, `createRole`, `updateRole`,
  `dropRole`, `shutdown`, `replSetReconfig`, `replSetStepDown`,
  `setParameter`, `setFeatureCompatibilityVersion`, `eval`, `fsync`,
  `compact`.
- **Unknown commands: default-deny** with `Unauthorized` (13) and a
  `slog.Info` line naming the command — the allowlist gets extended from
  real logs, not guessed. (Default-allow would silently punch holes in
  read_only.)
- **`$db` enforcement**: allow the configured `DatabaseName`; allow `admin`
  only for the diagnostics/always-allowed set; allow `$external` only for
  `saslStart`/`saslContinue`; deny `local` and `config`. `listDatabases` is
  denied by default (cluster-wide disclosure) — revisit later with
  result-filtering if users need it.
- `block_copy` has no Mongo equivalent (PG COPY-specific; MySQL ignores it
  too) — no mapping.

### Query logging & result capture (phases 2–3)

- `Query.SQLText` = command name + canonical Extended JSON of the kind-0
  body (truncated per `QueryStorageConfig`); `Parameters` may carry
  `lsid`/`txnNumber`.
- `RowsAffected` from reply `n`/`nModified`; result rows from
  `cursor.firstBatch`/`nextBatch`, each BSON doc re-encoded as Extended
  JSON into `QueryRow.RowData` — same role as
  `internal/proxy/mysql/result.go`.

### Mechanical checklist (from the MySQL precedent)

1. `internal/proxy/mongodb/` — server.go / session.go / auth.go /
   upstream.go / intercept.go / result.go / wire.go (framing per contract
   §1) / errors.go + tests.
2. `store.ProtocolMongoDB = "mongodb"` (`internal/store/models.go:116`) — no
   CHECK constraint exists on `databases.protocol`, so no migration unless
   we add columns (`auth_source`).
3. Config: `ListenMongo` + default `:27018` + `MongoConfig{TLS}` + env
   prefix rules (`internal/config/config.go`).
4. `main.go`: import, `startMongoProxy` (clone `startMySQLProxy`,
   `main.go:411`), add to `servers` shutdown slice.
5. OpenAPI (`internal/api/openapi.yml`): add `mongodb` to the three protocol
   enums (~lines 2315/2382/2424); `mongo_host`/`mongo_port` public-endpoint
   fields; `BuildConnectionURL` case producing
   `mongodb://user@host:port/db?authMechanism=PLAIN&authSource=<db>&tls=true&directConnection=true`
   (`internal/api/connection_url.go:47`); endpoint plumbing in
   `internal/store/global_parameters.go` + `internal/api/parameters.go`.
   Regenerate `front/src/api/schema.ts` (`bun run generate-client`).
6. Frontend: extend `Protocol` union + the four protocol maps + `SelectItem`
   in `front/src/routes/_authenticated/databases/index.tsx:66-91,307`;
   settings endpoint row in `settings/index.tsx`.
7. `docs/mongodb.md` following `docs/mysql.md` heading structure (the wire
   contract above seeds it).
8. Integration test cloning `internal/proxy/mysql/integration_test.go`
   `setupFixture` with `mongo:7` and `mongo:8` testcontainers.

### Testing requirements

- Integration tests dial **through** the proxy with the official Go driver
  (`authMechanism=PLAIN`, `directConnection=true`, TLS with
  InsecureSkipVerify) — the Go driver exercises the monitoring-connection
  path automatically, which is the point.
- Cases: connect+auth (password and API key), monitoring conn stays healthy
  for > heartbeat interval without auth, find/insert round-trip, read-only
  grant blocks insert/update/`$out` aggregate, block_ddl blocks
  createIndexes, `$db` violation denied, `w:0` write forwarded without
  reply, wrong password → code 18, session dump written, quota/revocation
  kills the connection mid-session.
- Manual smoke test with mongosh:
  `mongosh "mongodb://user:pass@localhost:27018/db" --authenticationMechanism PLAIN --authenticationDatabase db --tls --tlsAllowInvalidCertificates`
  — verify the connect-time diagnostic commands succeed (or fail cleanly)
  per the allowlist.

### Phasing (mirror the 2026-04-25 MySQL spec series)

1. **Phase 1 — auth + passthrough**: listener, TLS termination, wire framing
   (§1), lifecycle + synthesized hello (§2–§4), PLAIN client auth + SCRAM
   upstream (§5), verbatim relay (§6), connection recording, limits
   watchdog, dumps. No per-command parsing.
2. **Phase 2 — interception + enforcement**: parse OP_MSG sections, command
   classification, read_only/block_ddl/`$db` enforcement, query logging.
3. **Phase 3 — result capture**: cursor batch capture, rows-affected,
   getMore attribution.
4. **API/UI**: openapi enum + endpoints + frontend, connection-string
   builder.
5. **Later**: stored SCRAM verifiers in `protocol_data`, `loadBalanced`
   mode, OP_COMPRESSED, `listDatabases` result filtering.

### Known limitations (document in docs/mongodb.md)

- Transactions: refused client-side by drivers because we present as a
  standalone (transactions require replica-set/sharded topology). Retryable
  writes likewise disabled by drivers — both acceptable for an
  observability proxy.
- No wire compression, no streaming/awaitable hello, no exhaust cursors.
- PLAIN requires TLS; clients must pass
  `authMechanism=PLAIN&authSource=<dbname>&directConnection=true` (the
  authSource doubles as the dbbat database selector, contract §5).

---

## Implementation Plan

Structural template = `internal/proxy/mysql/`; hand-rolled framing = the pattern
in `internal/proxy/oracle/` + PG's `Peek`-based TLS negotiation
(`postgresql/session.go:725`). All shared plumbing reused verbatim
(`internal/proxy/shared`: CountingConn, LimitGuard/Watch, BuildUpstreamName,
validation).

### Dependencies
- `go get go.mongodb.org/mongo-driver/v2/bson github.com/xdg-go/scram github.com/xdg-go/stringprep`, `go mod tidy`. Promotes today's transitive deps to direct.

### Foundational wiring (keep binary green)
- `internal/store/models.go`: `ProtocolMongoDB = "mongodb"` (~:122). No CHECK
  constraint on `databases.protocol` → no migration. authSource resolved from the
  connection (`saslStart.$db`), default `admin`; no new column.
- `internal/dump/dump.go`: `ProtocolMongo = "mongodb"` (~:31).
- `internal/config/config.go`: `ListenMongo` default `:27018` (koanf `listen_mongo`,
  env `DBB_LISTEN_MONGO`); `MongoConfig{TLS TLSConfig}` field on Config; env prefix
  rule `mongo_tls_* -> mongo.tls.*`.
- `main.go`: import mongodb pkg; `startMongoProxy` (clone `startMySQLProxy` :411);
  wire into `runServer` gated on non-empty ListenMongo; append to `servers` slice.

### Phase 1 — internal/proxy/mongodb/
- `wire.go` — §1 framing: 16-byte header, OP_MSG (flag bits, kind-0/kind-1
  sections, trailing CRC-32C tolerated), OP_QUERY parse + OP_REPLY emit, reject
  OP_COMPRESSED. bson v2 for docs. `readMessage(io.Reader)`, `writeMessage`,
  helpers to build kind-0 reply. Unit-tested round trips.
- `server.go` — Server struct + deps (store, encryptionKey, queryStorage,
  dumpConfig, authCache, logger, tlsConfig); listener/accept loop; `Shutdown`;
  dump-cleanup ticker. Mirror `mysql/server.go`.
- `session.go` — §2 lifecycle: peek first byte 0x16 → TLS terminate else plaintext;
  PRE-AUTH loop answering hello/isMaster (§3 synthesized reply, §4 pinned
  maxWireVersion=21)/ping/buildInfo/endSessions WITHOUT store.Connection or
  upstream; on saslStart → auth. Monitoring conns served forever.
- `auth.go` — §5 SASL PLAIN server: parse `\0user\0pass`, verify via
  authCache.VerifyPassword or `dbb_` API-key path; target-db resolution order
  (saslStart.$db → user#db → single active grant → fail 18); on success resolve
  grant, register revocation, LimitGuard+Watch, CreateConnection, dial upstream.
- `upstream.go` — decrypt creds, dial (TLS per SSLMode), our hello with
  BuildUpstreamName app name, SCRAM-SHA-256 client via xdg-go/scram.
- CountingConn wrap + dump.NewTapConn after auth; verbatim relay (§6). `errors.go`
  §7 error docs (codes 18/13).

### Phase 2 — intercept.go
- Swap raw relay for parse→classify→forward. `ValidateMongoCommand(cmd, $db, body, grant)`
  in shared/validation.go: read/write/DDL/diagnostics/blocked lists; default-DENY
  unknown; aggregate w/ $out|$merge = write; `$db` rules. Log Query.SQLText =
  cmd + ExtJSON(kind-0 body), truncated.

### Phase 3 — result.go
- Capture cursor.firstBatch/nextBatch → QueryRow.RowData (ExtJSON); RowsAffected
  from n/nModified; getMore attributed to originating query.

### Phase 4 — API + UI
- openapi.yml: `mongodb` in 3 protocol enums; `mongo_host`/`mongo_port` on
  PublicEndpoints/ResolvedEndpoints/instance schemas.
- connection_url.go: `mongodb://user@host:port/db?authMechanism=PLAIN&authSource=<db>&tls=true&directConnection=true`.
- global_parameters.go + api/parameters.go: Mongo host/port params + resolved.
- `bun run generate-client`; front databases index Protocol union + 4 maps +
  SelectItem; settings endpoint row.

### Docs
- `docs/mongodb.md` mirroring `docs/mysql.md`.

### Tests
- `integration_test.go` (build tag `integration`) with mongo testcontainer, dial
  THROUGH proxy with official Go driver (PLAIN, authSource, directConnection, TLS
  skip-verify). Cases per Testing requirements.
