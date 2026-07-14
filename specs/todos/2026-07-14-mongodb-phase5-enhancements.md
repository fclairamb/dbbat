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
