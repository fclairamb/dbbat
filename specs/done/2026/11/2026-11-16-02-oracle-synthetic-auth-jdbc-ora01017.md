---
model: opus
effort: high
pr: batch
automerge: false
---

# The synthetic Oracle AUTH fallback cannot log a JDBC thin client in (ORA-01017)

GitHub Issue: https://github.com/fclairamb/dbbat/issues/500

## Source

Issue #500: the nightly `oracle (23ai Free (synthetic AUTH fallback))` leg
(`DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1`) fails every night. Its JDBC tests all die at login:

```
java.sql.SQLException: ORA-01017: dbbat accepted your API key, but the database refused the
credentials dbbat stores for it; ask a dbbat administrator to update them and run the
connectivity check
    at oracle.jdbc.driver.T4CTTIoauthenticate.doOAUTHWithO5Logon(T4CTTIoauthenticate.java:1845)
```

Failing: `TestIntegration_AsyncRefusalAgainstJDBCThin`, `TestIntegration_BlockedStatementRefusesJDBCThin`,
`TestIntegration_StatementTagFromJDBCThin` (ojdbc11 23.26.3) and
`TestIntegration_OJDBC6ThroughTheProxy` (ojdbc6 11.2.0.4). The same set fails on every run back to
at least 2026-10-03 (runs 37093905022, 39195137201, 40997082182, 43797954046): this is not a
regression, the synthetic fallback has never logged a JDBC client in since the leg exists. go-ora,
python-oracledb thin and sqlplus (wide synthetic builders) pass on the same leg.

The issue also lists `TestSendUpstreamAuthPhase{1,2}*_Wide` failing in 0.00 s on that leg: those
were unit tests of the *rewrite* run with the rewrite forced off, fixed on 2026-11-16 (they now
skip under the flag, `skipUnderForcedSyntheticAuth`).

## Problem

- `sendUpstreamAuthPhase1` / `sendUpstreamAuthPhase2`
  (`internal/proxy/oracle/upstream_auth_client.go:276`, `:344`) use the synthetic builders
  whenever `forceSyntheticUpstreamAuth` is set. On a thin session these are
  `buildClientAuthPhase1` / `buildClientAuthPhase2`, which emit a go-ora-shaped KV set.
- The upstream answers ORA-01017 (wrong credentials), not ORA-17401 / ORA-03120 (unparseable):
  the body parses but the O5LOGON proof does not verify. So the bytes dbbat computes for this
  session (AUTH_SESSKEY / AUTH_PASSWORD / AUTH_PBKDF2_SPEEDY_KEY, or the verifier type / salt it
  derives them from) differ from what the upstream expects when the *client* negotiated as JDBC.
  Candidates, none measured yet:
  - JDBC's Phase 1 asks for different logon-mode bits (`mode`) than go-ora, and the upstream's
    challenge (verifier 18453 vs 6949, PBKDF2 counts) depends on them;
  - the Phase 2 the synthetic builder sends lacks a key JDBC's Phase 1 committed to
    (AUTH_CONNECT_STRING / AUTH_ACL / the driver-version triplet, see the comment at
    `upstream_auth_client.go:328-332`), and 23ai refuses the login rather than the packet;
  - for ojdbc6 (TNS 310, legacy 2-byte lengths, 6949 challenge) the synthetic body uses a framing
    only go-ora ever exercised.
- `docs/oracle.md` "Exercising the synthetic AUTH fallback" says "Both runs must pass" without
  carving out JDBC, so the leg has been red by design since it was added.

## Why a spec

- Correctness hinges on authentication (the O5LOGON proof dbbat computes for the upstream).
- Needs a live 23ai and a JDBC client to measure; no unit-level reproduction exists.
- A product decision: whether the synthetic fallback has to support JDBC at all.

## Proposal

1. Reproduce: `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1 ORACLE_TEST_OJDBC_JAR=<ojdbc11.jar> go test -tags integration -p 1 -count=1 -run TestIntegration_StatementTagFromJDBCThin ./internal/proxy/oracle/`.
   Dump both bodies (the client's Phase 2 and the synthetic one) and the upstream's Phase 1
   challenge, and diff them key by key against the rewrite path, which logs the same client in.
2. Fix the synthetic builders for the delta found, with a unit test pinning the synthetic body
   against the captured JDBC Phase 1/2 (like `TestThinSyntheticAuthLeg_ShortValues_ByteIdentical`).
3. Or, if the delta is the JDBC-conditioned KV set (no way for a hand-built body to satisfy it
   without the client's packet), state it: skip the JDBC tests under
   `forceSyntheticUpstreamAuth` with that reason, and document in `docs/oracle.md` that the
   fallback supports go-ora / python thin / OCI shapes and that a JDBC session whose rewrite fails
   cannot log in.
4. Re-run the synthetic leg (`gh workflow run "Integration tests"`); it must be green.

## Open questions

- **Make the synthetic fallback work for JDBC, or scope it out?** Recommended: measure first
  (step 1); if the cause is a missing value dbbat can compute (mode bits, a verifier field), fix
  it; if it needs the client's own KV set, scope JDBC out (step 3). The fallback only runs when
  the rewrite fails, which no recorded JDBC client triggers, so scoping out costs nothing seen
  today; the trade-off is a JDBC client with an unusual Phase 2 failing hard instead of
  falling back.

## Closing the issue

This spec covers part of #500 (the synthetic-AUTH leg's JDBC tests). Issue #500 is already
closed by the fix commits of 2026-11-16 and the specs. The implementing PR body carries
`Refs #500`.

## Resolved open questions

- Measure first (step 1); if the cause is a missing value dbbat can compute, fix it; if it needs the client's own KV set, scope JDBC out of the synthetic fallback (step 3). (autopilot: recommended answer)
