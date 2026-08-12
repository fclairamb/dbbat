# The thin synthetic AUTH builders still write single-byte CLR chunks

## Goal

Thread the session's negotiated `UseBigClrChunks` through
`buildClientAuthPhase1` / `buildClientAuthPhase2` — the last two AUTH write
sites that still hard-code single-byte chunk lengths.

## Why

Three passes have now hardened the CLR long form:

- `2026-08-12-01` — the Phase 2 rewrite (`ttcClrVariant` / `ttcKeyValChunked`,
  `readCLRVariant`), thin path;
- `2026-08-12-07` — the client-facing challenge (`buildAuthChallenge`);
- `2026-08-12-08` — the whole wide/OCI leg, once the "OCI does not use it"
  premise turned out to be an assertion the sqlplus capture cannot support.

What is left is the **thin synthetic** upstream body:
`buildClientAuthPhase1` and `buildClientAuthPhase2`
(`internal/proxy/oracle/upstream_auth_client.go`) still encode every pair with
`ttcKeyVal`, i.e. `bigChunks=false`, even on a session where
`s.clientBigClrChunks` is true. Their wide counterparts
(`buildClientAuthPhase{1,2}Wide`) now take the flag, so the two dialects
disagree on the same session — the exact inconsistency the last three specs were
about.

Same reason it is not live: every value these builders emit is short —
`AUTH_SESSKEY` (64), `AUTH_PASSWORD` (96), `AUTH_PBKDF2_SPEEDY_KEY` (160),
hostnames, a PID, dbbat's own program name, the `ALTER SESSION` string — and
short values are byte-identical in both encodings. It becomes reachable the
moment one of those outgrows the 252-byte short form (a long hostname in
`AUTH_TERMINAL` / `AUTH_MACHINE` is the most plausible route, at 253 bytes).

No GitHub issue yet — file one when picking this up.

## Implementation

- Give `buildClientAuthPhase1` / `buildClientAuthPhase2` a `bigChunks bool` and
  swap `ttcKeyVal` for `ttcKeyValChunked(..., bigChunks)`. The username stays on
  `ttcClr`: Oracle caps an identifier at 128 bytes, half the short-form limit.
- Both call sites are session methods in `sendUpstreamAuthPhase1` /
  `sendUpstreamAuthPhase2`, so `s.clientBigClrChunks` is already in hand — the
  wide branches next to them read it today.
- Test alongside `TestWideAuthLeg_ShortValues_ByteIdentical`
  (`internal/proxy/oracle/clr_bigchunks_wide_test.go`): today's pair sets must be
  byte-identical with the flag on and off, and a synthetic over-long value must
  round-trip through `readAuthKVPair(_, false, true)`.

## Key files

- `internal/proxy/oracle/upstream_auth_client.go`
  (`buildClientAuthPhase1`, `buildClientAuthPhase2`, `sendUpstreamAuthPhase1`,
  `sendUpstreamAuthPhase2`)
- `internal/proxy/oracle/ttc_auth.go` (`ttcKeyValChunked`)
- `internal/proxy/oracle/clr_bigchunks_wide_test.go`
