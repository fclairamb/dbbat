---
model: opus
effort: high
---
# MongoDB proxy — phase 5 enhancements (deferred)

## Goal
Follow-up work deliberately deferred from the initial MongoDB proxy
implementation (`specs/done/2026/07/2026-07-14-mongodb-support.md`, phases
1–4, shipped 2026-07-14). None of these block the proxy from working — they
remove limitations documented in `docs/mongodb.md` "Known limitations".

## Why
Phases 1–4 landed a working, audited MongoDB proxy (PLAIN-over-TLS client
auth, SCRAM upstream, command classification/enforcement, query + result
logging, API/UI, testcontainer integration test). The items below were scoped
out as "Later" in the original spec and are captured here so they aren't lost.

No GitHub issue yet — file one when picked up.

## Implementation

Pick these up independently; each is self-contained.

1. **Stored SCRAM verifiers** (`users.protocol_data` jsonb, `"mongodb"` key) so
   clients can use the driver-default `SCRAM-SHA-256` instead of being forced
   onto `authMechanism=PLAIN`. Mirror the Oracle O5LOGON verifier precedent
   (`internal/store/models.go` `protocol_data`, migration
   `20260712210000_users_protocol_data`). Verifiers only exist for passwords
   set after this ships; keep PLAIN as the fallback.

2. **Configurable upstream `authSource`.** The initial implementation fixes the
   upstream SCRAM `authSource` to `admin` (`internal/proxy/mongodb/upstream.go`).
   Make it a per-database setting — either a nullable `auth_source` column on
   `databases` (with migration) or a `protocol_data` entry — surfaced in the
   create/update API + form. Default `admin`.

3. **`loadBalanced=true` mode** (MongoDB 5.0+): reply to `hello` with a
   `serviceId` so drivers pin cursors/transactions to the connection. Designed
   exactly for L4 proxies; a cleaner topology story than `directConnection=true`.

4. **OP_COMPRESSED support** (opcode 2012): negotiate a compressor in `hello`
   and inflate/deflate around the existing OP_MSG parse. Currently rejected
   (`internal/proxy/mongodb/session.go`). Lets compression-on clients connect.

5. **`listDatabases` result filtering:** currently denied outright (cluster-wide
   disclosure). Allow it but filter the returned `databases` array to the grant's
   configured database. Requires parsing + rewriting the upstream reply.

6. **find→getMore lineage linking.** Result capture logs each `getMore` batch
   against its own request; link a `getMore` back to the originating
   `find`/`aggregate` cursor so a full result set reads as one logical query.

7. **Test matrix:** extend `internal/proxy/mongodb/integration_test.go` to run
   against `mongo:8` in addition to `mongo:7` (parametrize `MONGO_TEST_IMAGE`),
   and add an explicit `w:0` / `moreToCome` case asserting no reply is produced.

## Implementation Plan

All 7 items are implemented on the current batch branch. Design decisions and
touch points, item by item:

### 1. Stored SCRAM verifiers (`users.protocol_data.mongodb`)
- **Model** (`internal/store/models.go`): add `MongoDB *MongoUserData` to
  `UserProtocolData`; `MongoUserData.SCRAMSHA256 *MongoSCRAMCredentials{Salt
  []byte (public), Iterations int, StoredKey []byte (enc), ServerKey []byte
  (enc)}`. Add `User.MongoData()` accessor, mirroring `OracleData()`.
- **Derivation** (`internal/store/mongo_scram.go`): compute the SCRAM-SHA-256
  stored credentials from a plaintext password via `xdg-go/scram`
  (`SHA256.NewClient(...).GetStoredCredentials(KeyFactors{Salt, Iters:15000})`),
  random 16-byte salt. Store method `SetUserMongoVerifier(ctx, userID,
  password, encKey)` encrypts StoredKey/ServerKey (AAD = `crypto.UserAAD(uid)`)
  and persists into `protocol_data.mongodb`, preserving other protocols.
- **crypto**: add `UserAAD(userUID)`.
- **API hooks**: after every password set, persist the verifier — create user,
  update user password (`internal/api/users.go`), self change-password, admin
  reset, first-login change (`internal/api/auth.go`). Best-effort (logged, never
  fails the request) since it is an additive optimisation.
- **Proxy** (`internal/proxy/mongodb/scram_server.go`): server side of
  SCRAM-SHA-256 via `xdg-go/scram` `NewServer`; the credential lookup resolves
  the dbbat user by SCRAM username and decrypts their stored keys. saslStart
  dispatches on `mechanism` (`PLAIN` → existing; `SCRAM-SHA-256` → new,
  multi-step). `helloDoc` advertises `SCRAM-SHA-256` in `saslSupportedMechs`
  only when the named user has a stored verifier; PLAIN stays the fallback.
  Refactor `handleSaslStart`/`completeAuth` into a shared `authorizeSession`
  (resolve DB + grant + quotas + upstream dial + record) reused by both paths.

### 2. Configurable upstream `authSource`
- **Migration** `20260714120000_databases_mongo_auth_source`: nullable
  `mongo_auth_source text` column (mirrors nullable `oracle_service_name`).
- **Model/store**: `Database.MongoAuthSource *string`, `DatabaseUpdate`,
  create/update handling.
- **API** (`internal/api/databases.go` + `openapi.yml`): request/response field
  `mongo_auth_source`; **Frontend** (`front/`): auth-source input shown for the
  mongodb protocol; regenerate `schema.ts` field.
- **Proxy** (`upstream.go`): `scramAuthDB()` returns `db.MongoAuthSource` when
  set, else `admin`.

### 3. `loadBalanced=true`
- **hello.go**: when the client's hello carries `loadBalanced:true`, include a
  stable per-server `serviceId` (`bson.ObjectID`, generated once in
  `NewServer`). Documented as the preferred topology story for L4 proxying.

### 4. OP_COMPRESSED (opcode 2012)
- **wire.go**: parse/build OP_COMPRESSED (originalOpcode, uncompressedSize,
  compressorId, data); support `noop` (0) + `zlib` (2) (both stdlib).
- **hello**: advertise `compression:["zlib"]` when the client offered it.
- **session/intercept**: transparently decompress inbound OP_COMPRESSED into a
  logical message before classification; forward the *decompressed* frame
  upstream (we never negotiate compression upstream). Mirror the client:
  compress OP_MSG/OP_REPLY replies only once the client has itself sent a
  compressed frame (guarantees it will decompress ours; keeps the first hello
  reply plain).

### 5. `listDatabases` result filtering
- **validation.go**: `classListDatabases` becomes allowed (skips the `$db`
  admin restriction) instead of denied.
- **result path** (`result.go`/`session.go`): when the pending command for a
  reply is `listDatabases`, rewrite the reply's `databases` array to only the
  grant's target db (`db.DatabaseName`), recompute `totalSize`/`totalSizeMb`,
  and relay the rebuilt frame (no longer verbatim).

### 6. find→getMore lineage linking
- In-session `cursorOrigins map[int64]origin`. On a `find`/`aggregate`/list
  reply opening a cursor (`cursor.id != 0`), record the origin command +
  namespace and stamp the origin's own log `Parameters` with `cursor_id=<id>`.
  On a `getMore`, resolve the cursor id → origin and stamp the getMore log with
  `cursor_id` + `cursor_origin=<cmd> <ns>`, so every batch of one result set
  shares a `cursor_id`. Drop the mapping when a reply returns `cursor.id == 0`.
  No schema change.

### 7. Test matrix
- `MONGO_TEST_IMAGE` parametrization already exists; document running against
  `mongo:8`. Add a `w:0` unacknowledged-write (`moreToCome`) integration test
  asserting the write is logged with no reply expected, plus unit coverage for
  OP_COMPRESSED round-trips, SCRAM verifier derivation, and listDatabases
  filtering.

`docs/mongodb.md` "Known limitations" is trimmed as each limitation is lifted.
