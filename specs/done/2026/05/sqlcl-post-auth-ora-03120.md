# SQLcl post-auth ORA-03120

## Goal

Make the first user query through SQLcl 26.1 reach the upstream Oracle and return rows. Today auth completes (PBKDF2 path), the proxy enters relay mode, and Oracle replies to the very first query with two TNS Marker (interrupt) packets followed by an OER carrying:

> ORA-03120: two-task conversion routine: integer overflow

This is the next blocker after `sqlcl-client-validation.md`'s auth gap, which is now resolved.

## Symptom on the wire

Captured 2026-04-29 via `DBB_DUMP_DIR=/tmp/dbbat-dumps`. First post-AUTH packet, SQLcl → upstream, 469 bytes:

```
0:  00 00 01 d5 06 00 00 00 08 00 03 5e 03 02 80 21 00 01 02 01 83 01 01 0d 00 00 04 ff ff ff ff 01
32: 0a 04 7f ff ff ff 00 00 00 00 00 00 00 00 00 00 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
64: 00 20 73 65 6c 65 63 74 ...   "select parameter,value from nls_session_parameters union all ..."
```

Upstream response sequence:

```
#2  S→C 11 bytes  0000000b 0c200000 010001        ← Marker (interrupt)
#3  S→C 11 bytes  0000000b 0c200000 010002        ← Marker (reset)
#4  C→S 11 bytes                                   ← SQLcl marker reply
#5  S→C 105 bytes containing "ORA-03120: two-task conversion routine: integer overflow"
```

Markers mean Oracle is interrupting *mid-parse*, not refusing on auth state. python-oracledb thin works fine through the same dbbat instance.

## Root cause

TNS framing is identical to python-oracledb. There is **no** extra `0x00` byte before the TTC function code, and the data flag (`0x0800`, `TNS_DATA_FLAGS_END_OF_REQUEST`) is a documented value. The earlier hypothesis about a "JDBC 0x0008 framing extension" was based on a hex-column-boundary misread and turned out to be wrong — it's recorded so we don't go down that path again.

The real divergence is **inside the OALL8 piggyback body**, reflecting different TTC compile-time caps:

| Field                | python-oracledb | SQLcl (JDBC thin 23.x)  |
|----------------------|-----------------|-------------------------|
| Cursor option byte   | `0x61`          | `0x21`                  |
| Options/flags run    | `01 18 01 01`   | `02 01 83 01 01`        |
| SQL length encoding  | 1-byte `0x18`   | 2-byte `00 20`          |

Why upstream chokes: the pre-auth relay forwards Connect / Accept / Set Protocol / Set Data Types directly between SQLcl and the real upstream so SQLcl gets responses tailored to its own caps. Then dbbat **closes** that socket and `upstreamGoOraAuth` opens a fresh go-ora connection, which negotiates `CompileTimeCaps[7]=11` from `data_type_nego.go`. The go-ora-negotiated upstream socket is at a different cap level than the one SQLcl thinks it's talking to, so SQLcl's OALL8 encoding doesn't parse and Oracle returns ORA-03120 on the first query.

## Paths forward

Each is multi-day; pick one.

### A. Carry the relay-phase upstream socket through AUTH (chosen — implemented 2026-05-01)

Don't close `upstream` in `relayPreAuthNegotiation`. After dbbat completes O5LOGON with the client (using the API key), inject AUTH Phase 1 / 2 toward the *same* upstream socket using stored DB credentials. Caps stay aligned automatically — Oracle parses SQLcl's bytes the way SQLcl encoded them.

Cost: implement an O5LOGON CLIENT in dbbat that can drive an existing TNS connection, including the PBKDF2 (verifier 18453) path and the `customHash` capability bit. `go-ora/v2/auth_object.go` is the canonical reference.

**Implementation summary (2026-05-01):**

- `relayPreAuthNegotiation` now returns the upstream socket alive across the AUTH boundary, plus an `upstreamNego` snapshot of the ServerCompileTimeCaps the upstream advertised. The client-facing customHash strip stays in place so dbbat's O5LOGON server (legacy MD5/XOR) keeps working.
- New `internal/proxy/oracle/o5logon_client.go` drives AUTH Phase 1 / Phase 2 over that socket. Implements verifier 18453 (PBKDF2 HMAC-SHA512 / SHA-512 password key) with the customHash combined-key derivation, plus the legacy 6949 (SHA-1) path for older databases. Mirrors `go-ora/v2 auth_object.go` exactly for the wire-level KV layout, magic bytes (`03 76 00 01` Phase 1, `03 73 00` Phase 2), and PKCS7 padding semantics.
- `upstreamAuth` tries the new path first; on any error it falls back to the legacy go-ora-fresh-socket path with the captured AUTH OK — preserving today's behaviour for existing clients.
- The upstream's real AUTH OK packet is forwarded verbatim to the dbbat client, so clients that validate `AUTH_SVR_RESPONSE` (python-oracledb thin, SQLcl) get session-specific data that matches their derived combined key.
- 30+ unit tests added covering the TTC cursor (compressed-int / CLR / DLC / KV), Phase 1/Phase 2 wire shape, the AES-CBC + PBKDF2 chain, and a full crypto-only end-to-end roundtrip for verifier 18453.

### B. Transcode OALL8 piggyback bodies in `proxyMessages`

Decode SQLcl's OALL8 payload, re-encode in go-ora's lower-cap format on the way to upstream, do the inverse for responses. Most code by far — needs a full OALL8/QueryResult encoder/decoder for both directions, plus `OFETCH`, `OCLOSE`, continuation packets, error responses, etc.

### C. Force go-ora to advertise SQLcl's caps

Would only work if (1) we can observe SQLcl's negotiated caps during the relay and (2) go-ora exposes a way to override its `CompileTimeCaps` before opening. Neither is currently true; option C is on the list for completeness but is the weakest.

## Acceptance criteria

- [x] Code path implemented and unit-tested end-to-end (crypto chain, wire shape, parser).
- [x] go-ora end-to-end through dbbat: validated locally against Oracle 19c (`abyla_abynonprod`); query intercepted, result returned.
- [x] python-oracledb thin end-to-end through dbbat: validated locally — uses the relay-socket path + per-session AUTH_SVR_RESPONSE patch; previous DPY-4035 resolved.
- [ ] **SQLcl end-to-end through dbbat: still blocked.** Pre-auth relay completes; the upstream then sends two TNS Marker (interrupt + reset) packets in response to dbbat's hand-built Phase 1. Both go-ora and python-oracledb work because the TTC caps the upstream negotiated with them match dbbat's own Phase 1 wire shape; SQLcl negotiates richer JDBC-thin caps so the upstream rejects dbbat's go-ora-style Phase 1 mid-parse. The fix is **Phase-1 forwarding**: take SQLcl's actual Phase 1 packet, swap the username field to the stored DB user, and forward as-is. Roughly the approach of the dropped 5294b37 commit, but kept minimal.
- [x] The Oracle compatibility table in `docs/oracle.md` updated.
- [x] python-oracledb thin no-regression: validated locally end-to-end.

## Update 2026-05-07: regression fix landed; SQLcl still blocked at upstream Phase 1

While reproducing the SQLcl failure locally, three regressions in the upstream-auth parser were found and fixed (`internal/proxy/oracle/upstream_auth_client.go`):

1. `parseAuthKVDictionary` was reading the dictionary length as a **raw 2-byte big-endian uint** when Oracle's wire format is a TTC compressed integer. dbbat read `01 06` as 262 instead of 6 and tried to consume 262 KV pairs.
2. `parseAuthSummary` treated the first compressed-int after code 0x04 / 0x09 as the Oracle error code. That field is actually `EndOfCallStatus` — for Phase 1 challenge responses Oracle sets it to 1 (continuation needed) with `RetCode=0`. dbbat misreported the success as ORA-00001.
3. `parseAuthMessageStream` waited for an end-of-call code 0x04 / 0x09 after the 0x08 dictionary, but Phase 2 responses interleave additional TTC messages (notably code 0x17 server-network-info) before the trailing 0x04. The new logic returns success after consuming the first 0x08 dictionary, which carries everything we need for either phase.

Plus a fourth gap that python-oracledb thin and SQLcl both validated and rejected: dbbat was sending a static `capturedAuthOKResponse` blob to the client. Modern clients verify the embedded AUTH_SVR_RESPONSE field by AES-decrypting it under the negotiated O5LOGON combined key — and the captured blob's value was encrypted under a different key. The fix forwards the upstream's real AUTH OK packet with AUTH_SVR_RESPONSE re-encrypted under the client's combined key (`internal/proxy/oracle/auth_svr_response.go`).

Validated locally against Oracle 19c (`oracle-abynonprod.db.stonal.io`):

| Client | Before | After |
|--------|--------|-------|
| go-ora v2.9.0 | hangs 60s, upstream RST (`read: connection reset by peer`) | works |
| python-oracledb thin 3.4 | DPY-4035 (invalid server response) | works |
| SQLcl 26.1 (JDBC thin 23.7) | ORA-17401 | upstream sends TNS Markers in response to Phase 1; same end-state but the failure now lives at upstream Phase 1 acceptance, not AUTH OK validation |

The SQLcl path forward is now just **Phase-1 forwarding** (not the broader "find the right verifier-6949+customHash derivation" rabbit hole that the original spec sketched). Picking up that work means: capture SQLcl's Phase 1 from the relay handoff (already available as `phase1Pkt`), locate the username field, splice in the upstream DB user, write the modified packet to the upstream socket. The Phase 2 path is unchanged because dbbat continues to drive Phase 2 with its own crypto.

## Update 2026-05-07 (later): Phase-1 forwarding shipped, SQLcl still blocked — back to verifier-6949+customHash

Phase-1 forwarding landed in #138 (`internal/proxy/oracle/phase1_forward.go`,
`rewriteAuthPhase1Username`) and works: the upstream now accepts SQLcl's
Phase 1 wire shape (with the username swapped to the upstream DB user)
and returns a normal challenge. python-oracledb thin and go-ora are
unaffected (no regression).

But the **rabbit hole the earlier update declared closed turned out to be
the actual root cause for SQLcl**:

| Captured upstream Phase 1 response | Verifier | Outcome |
|------------------------------------|----------|---------|
| 363 bytes — includes `AUTH_PBKDF2_CSK_SALT` / `_VGEN_COUNT` / `_SDER_COUNT` | 18453 | dbbat completes Phase 2; upstream returns AUTH OK; client rejects (next layer) |
| 261 bytes — no `AUTH_PBKDF2_*` fields | 6949 with `customHash=true` | dbbat's `derivePasswordEncKey` runs PBKDF2 with an empty `pbkdf2ChkSalt` → wrong key → upstream rejects Phase 2 with two TNS Markers (interrupt + reset) and RSTs the connection |

The same upstream / same DB user returns verifier 6949 most of the time
and verifier 18453 occasionally; the trigger has not been pinned down
(possibly tied to whether ANO / TLS handshake bits ride along — JDBC
thin's `anoEnabled=true` may switch it). Both cases need to work end-to-end.

For the verifier-18453 path, SQLcl gets to AUTH OK validation. Captured
JDBC stack via the ojdbc11 harness is `T4CTTIfun.receive:1048` →
`T4CTTIfun.doRPC:237` → `T4CTTIoauthenticate.doOAUTH:547` —
JDBC's protocol-violation default case — but no FINEST trace lines
reveal which message-code byte trips it. python-oracledb thin accepts
the same forwarded AUTH OK without complaint.

For the verifier-6949+customHash+no-PBKDF2 path, JDBC source / bytecode
reverse-engineering (`T4CTTIoauthenticate.doOAUTHWithO5Logon` is the
target method) is the unblocker. Whatever combined-key derivation JDBC
uses in this case, dbbat (and go-ora v2 — both implement the same
algorithm) does not. `~/go/pkg/mod/github.com/sijms/go-ora/v2@v2.9.0/auth_object.go`
line 460 onwards is the canonical reference for the path dbbat copies;
the divergence from JDBC must live in the `customHash & verifier=6949 & no PBKDF2 fields` corner.

### Reproducing today

```bash
# Build + run dbbat in test mode (DBB_LOG_LEVEL=debug surfaces every
# upstream AUTH packet's hex_full so you can compare verifier-18453 and
# verifier-6949 traces side by side)
go build -o /tmp/dbbat .
DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable" \
  DBB_RUN_MODE=test DBB_LOG_LEVEL=debug \
  DBB_LISTEN_ORA=":1522" DBB_LISTEN_API=":4200" DBB_LISTEN_PG=":5434" \
  /tmp/dbbat &

# Provision (admin/admintest, oracle-abynonprod, LABEOMNGR_DEV)
# Then drive each client:
go run /tmp/gooracheck.go             # PASS
python3 -c "import oracledb; ..."     # PASS
sql -S connector/dbb_connector_key@localhost:1522/abynonprod   # FAIL (ORA-17401)

# Bytecode reverse-engineering recipe for the JDBC handler:
mkdir -p /tmp/jdbc-extract && cd /tmp/jdbc-extract
JDBC=/opt/homebrew/Caskroom/sqlcl/26.1.0.086.1709/sqlcl/lib/ojdbc11.jar
jar xf $JDBC oracle/jdbc/driver/T4CTTIoauthenticate.class
javap -c -p oracle/jdbc/driver/T4CTTIoauthenticate.class > /tmp/oauth.javap
# doOAUTHWithO5Logon at line 1746 is where the password-key derivation
# happens; cross-reference against oracle/jdbc/util/Hash.class etc.
```

## Local validation setup (2026-05-06)

For follow-up debugging, the working environment is:

```bash
# Postgres for dbbat storage
docker compose up -d postgres
docker exec dbbat-postgres psql -U postgres -c \
  "DROP SCHEMA public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO postgres; GRANT ALL ON SCHEMA public TO public;"

# Oracle credentials from the dev pod (SSM was stale per db-provision.priv.md)
kubectl --context=aws/nonprod -n dev exec service-abyla-<pod> -- env | grep ORACLE_DATABASE_PASSWORD

# Build + run dbbat in test mode
go build -o /tmp/dbbat .
DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable" \
  DBB_RUN_MODE=test DBB_LOG_LEVEL=debug \
  DBB_LISTEN_ORA=":1522" DBB_LISTEN_API=":4200" DBB_LISTEN_PG=":5434" \
  DBB_DUMP_DIR=/tmp/dbbat-dumps /tmp/dbbat &

# Provision via API: connector test API key auto-creates with O5LOGON verifier
curl -s -u admin:admintest -X POST http://localhost:4200/api/v1/databases \
  -H 'Content-Type: application/json' \
  -d '{"name":"abynonprod","host":"oracle-abynonprod.db.stonal.io","port":1521,
       "protocol":"oracle","oracle_service_name":"TEST01","database_name":"TEST01",
       "username":"LABEOMNGR_DEV","password":"<from-pod-env>","ssl_mode":"disable"}'
# (then create a grant for the connector user)

# Run clients
go run main.go                             # go-ora: PASS
python3 -c "import oracledb; ..."          # python-oracledb thin: PASS
/opt/homebrew/Caskroom/sqlcl/.../bin/sql -S \
  connector/dbb_connector_key@localhost:1522/abynonprod  # SQLcl: ORA-03120 / ORA-17401
```

## Next steps for SQLcl (path forward)

Phase-1-rewrite is implemented (`rewriteAuthPhase1Username` in
`internal/proxy/oracle/o5logon_client.go`). The upstream now sees dbbat's Phase
1 as if it came from the original client (same data flag, same encoding,
username swapped) and replies with a normal challenge. The blocking issue has
moved to **Phase 2 password-key derivation for SQLcl's negotiated verifier**.

The remaining work falls into two complementary pieces:

### A. Recover the verifier-6949+customHash key derivation Oracle expects

Empirically the upstream returns:
- verifier_type = 6949
- caps[4] & 0x20 (customHash) = set
- no AUTH_PBKDF2_CSK_SALT / VGEN_COUNT / SDER_COUNT in the challenge

dbbat currently falls back to the legacy MD5/XOR derivation when PBKDF2 fields
are absent (see `derivePasswordEncKey`). The upstream rejects that derivation
with end-of-call code 4. There must be a different combined-key formula
JDBC-thin uses for this case. Capture it via:

1. Run a stripped-down Java client against the upstream directly (no dbbat),
   per the `feedback_jdbc_oracle_trace.md` recipe (15-line Java program with
   ojdbc11.jar).
2. Add `-Doracle.jdbc.Trace=true -Doracle.net.crypto.checksum.types=NONE
   -Doracle.net.crypto.types=NONE` if needed to disable encryption layers.
3. Capture the wire bytes and the password-encryption key with `T4CTTIoauthenticate`
   instrumentation.
4. Compare against go-ora's path to identify the missing transformation.

### B. Patch additional AUTH OK fields if SQLcl checks more than AUTH_SVR_RESPONSE

Even with the captured-AUTH-OK fallback, SQLcl returns ORA-17401 (Protocol
violation), matching the 2026-04-29 attempt note (`da62fde`). Other session-
data fields likely need patching too (AUTH_VERSION_STRING, AUTH_VERSION_NO,
AUTH_DBNAME, AUTH_INSTANCE_NO, …). A Java stack trace (same recipe as A) would
pinpoint which field SQLcl rejects.

## Artifacts

- `/tmp/dbbat-dumps/019dd9e0-fce1-78a2-8e25-5e5b204d06a7.dbbat-dump` — SQLcl 26.1 session this turn.
- `/tmp/sqlcl-final-tap.log` — earlier SQLcl tap.
- `/tmp/pyora-tap.log` — python-oracledb working reference for comparison.
- `/tmp/parse_dump.py` — working v2 dump parser (the in-repo `scripts/replay_dump.py` skips the JSON-header-length read and crashes on v2 dumps; that's a separate one-line fix).

## Related

- `specs/todos/sqlcl-client-validation.md` — predecessor blocker (auth itself); resolved 2026-04-29 by the PBKDF2 / customHash work in `43ef6ea` and `aecb032`.
- `~/.claude/projects/-Users-florent-code-fclairamb-dbbat/memory/feedback_oracle_pbkdf2.md` — PBKDF2 reference notes.
- `~/.claude/projects/-Users-florent-code-fclairamb-dbbat/memory/feedback_sqlcl_post_auth.md` — short-form summary of the misread + correct diagnosis.

## Implementation Plan

Going with **Path A**: keep the relay-phase upstream socket alive through AUTH, then run an O5LOGON CLIENT on it as the database user. Caps stay aligned automatically.

### 1. Wire the relay socket through to upstreamAuth

- Change `relayPreAuthNegotiation` to return `(*TNSPacket, net.Conn, bool, error)` — the AUTH Phase 1 packet, the still-open upstream socket, and a `customHash` bit observed in the Set Protocol response (caps[4]&0x20 before stripping). Drop the `defer upstream.Close()`.
- Update `session.run` to receive these and store the upstream socket on `s.upstreamConn` before calling `upstreamAuth`.
- Track customHash as a field on `session` so the AUTH client can switch between the legacy 6949 path and the modern 18453/PBKDF2 path.

### 2. Implement Oracle AUTH-as-client crypto

New file `internal/proxy/oracle/upstream_auth_client.go` with helpers that mirror `go-ora/v2/auth_object.go`:

- `pbkdf2SpeedyKey(buffer, key, turns)` — HMAC-SHA512 chained over the buffer, XORing intermediates (matches `generateSpeedyKey` lines 323–339 of `auth_object.go`).
- `derivePBKDF2VerifierKey(password, salt, vgenCount)` — `key = SHA512(speedyKey || salt)[:32]` for verifier 18453.
- `decryptServerSessKey` / `encryptClientSessKey` — AES-CBC with zero IV, padding off for 18453 (truncate to original length on encrypt).
- `derivePasswordEncKey` — for customHash + 18453, HMAC-SHA512 chain over hex(client||server) with key=pbkdf2ChkSalt for sderCount turns; truncate to 32 bytes.
- `encryptPassword(password, key, padding)` — prepend 16 random bytes, AES-CBC encrypt, optionally truncate.

### 3. Implement Oracle AUTH-as-client wire path

Same file, but the protocol layer:

- `buildClientAuthPhase1(username, mode)` — TTC body: `03 76 00 01` + compressed userLen + compressed mode (NoNewPass) + `01 01 05 01 01` + CLR(username) + KV pairs (`AUTH_TERMINAL`, `AUTH_PROGRAM_NM`, `AUTH_MACHINE`, `AUTH_PID`, `AUTH_SID`).
- `parseAuthPhase1Response(payload)` — read TTC message codes; on `0x08`, read 4-byte BE dictLen and consume that many KV pairs; capture `AUTH_SESSKEY`, `AUTH_VFR_DATA` (and its flag → VerifierType), `AUTH_PBKDF2_CSK_SALT`, `AUTH_PBKDF2_VGEN_COUNT`, `AUTH_PBKDF2_SDER_COUNT`. Stop on code 4 or 9 (end of response).
- `buildClientAuthPhase2(authObj, username, mode)` — TTC body: `03 73 00` + `01` + 4-byte BE userLen + 4-byte BE mode (with `UserAndPass|NoNewPass`) + `01` + 4-byte BE pairCount + `01 01` + CLR(username) + KV pairs starting with `AUTH_SESSKEY` (encrypted client session key, flag=1), then `AUTH_PASSWORD`, optionally `AUTH_PBKDF2_SPEEDY_KEY`, then the standard `AUTH_TERMINAL` etc. plus an `AUTH_ALTER_SESSION` with NLS settings.
- `parseAuthPhase2Response(payload)` — loop on TTC message codes; on code 8, read dictLen and KV pairs (we mostly ignore them but skipping their bytes is needed); detect ORA error codes if message code != 4/9; succeed when we see code 4 or 9.

### 4. New `runUpstreamClientAuth(s)` function

- Top-level orchestrator: build phase 1, write as TNS Data, read upstream packet, parse phase 1 response, compute keys (handle VerifierType 6949 OR 18453 to be defensive), build phase 2, write, read responses, return success or wrap an Oracle error.
- Reuse the existing `encodeV315DataPacket` for framing and `readTNSPacket` for upstream reads.
- Multi-packet / continuation responses: keep reading TNS packets until we see code 4 or 9 in any of them.

### 5. Replace upstream auth wiring

- Replace `upstreamGoOraAuth` / `extractGoOraConn` with `runUpstreamClientAuth(s)`.
- Drop `s.goOraConn` (no more reflection-based unsafe access). Cleanup just closes `s.upstreamConn`.
- Remove the `goora` dependency on the auth path. The library is still imported elsewhere — leave the dependency.

### 6. Tests

- `upstream_auth_client_test.go`:
  - Unit: `pbkdf2SpeedyKey` against a hand-computed vector from `auth_object.go`.
  - Unit: AES key derivation for verifier 18453 against a captured (salt, password, vgen) tuple if available, otherwise a synthetic one.
  - Unit: `buildClientAuthPhase1` produces a byte sequence matching the documented preamble.
  - Round-trip: `buildClientAuthPhase2` followed by re-parsing on the dbbat server side recovers AUTH_SESSKEY (proves we and our own server agree on the format).
- Smoke test using a fake upstream that mimics Oracle's AUTH response: verifies our parser walks the message stream correctly.

### 7. Documentation

- `docs/oracle.md`: update the compatibility table to mark SQLcl 26.1 as working end-to-end; note the new auth-on-relay-socket flow under "Authentication path"; remove the SQLcl ORA-17401 / ORA-03120 known-limitation paragraphs.
- Bump the related "Authentication path" section so the customHash strip is described as client-facing only (we keep the upstream socket's view of customHash intact for our own AUTH).

### 8. Regression check

- Run `make test` (covers unit + testcontainer Oracle integration).
- Confirm python-oracledb thin still works through the new flow — its existing integration test path doesn't change because dbbat-side caps are unchanged.
