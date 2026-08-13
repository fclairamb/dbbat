# `buildAuthChallenge` writes the client-facing AUTH challenge with single-byte CLR chunks

## Goal

Make the challenge dbbat sends *to the client* honour the session's negotiated
CLR long form, the same way the Phase 2 rewrite now does on the upstream leg.

## Why

`2026-08-12-01-oracle-ttckeyval-ignores-bigclrchunks` hardened every write path
in the Phase 2 rewrite (`ttcClrVariant` / `ttcKeyValChunked`, threaded from
`session.clientBigClrChunks`). It deliberately did not touch
`buildAuthChallenge` in `internal/proxy/oracle/ttc_auth.go`, which is the other
direction: the AUTH challenge dbbat synthesizes for the client in terminated-auth
mode. It still encodes every pair with `ttcKeyVal` / `ttcKeyValWide`, i.e. the
0xFE long form with single-byte chunk lengths.

Same latent bug, same reason it is not live: every value in the challenge is
short — `AUTH_SESSKEY` (64 hex chars), `AUTH_VFR_DATA`, `AUTH_PBKDF2_CSK_SALT`,
the two PBKDF2 counts, `AUTH_GLOBALLY_UNIQUE_DBID` — and short values are
byte-identical in both encodings. It becomes reachable if a verifier or salt
size ever crosses 252 bytes.

No GitHub issue yet — file one when picking this up.

## Implementation

- `buildAuthChallenge` currently selects its encoder by assigning
  `kv := ttcKeyVal` (or `ttcKeyValWide`) as a func value, which is why the flag
  was not threaded in the first pass. Either close over `bigChunks`
  (`kv := func(k, v string, f int) []byte { return ttcKeyValChunked(k, v, f, bc) }`)
  or take the encoder from the caller.
- Thread `s.clientBigClrChunks` from the call sites — grep
  `buildAuthChallenge(` in `internal/proxy/oracle/`.
- The wide/OCI leg should stay on single-byte chunks: `readAuthKVPairWide` and
  `replaceAuthKVValueWide` both read plain `readCLR`, so that framing is settled
  independently of the capability.
  **Superseded by `2026-08-12-08-oracle-wide-leg-bigclrchunks-unverified`**: that
  premise was an assertion, and the sqlplus capture does not support it — the OCI
  session negotiates `UseBigClrChunks` like any other and carries no long-form
  value at all, so nothing in `testdata/` settles the framing. Both readers now
  honour the flag, and so does the wide branch of `buildAuthChallenge`. The
  byte-identity test this spec added still holds and still means what it said:
  for today's short values the flag moves nothing.
- Test alongside `TestTTCClrVariant_ShortValue_NoOp` in
  `internal/proxy/oracle/clr_bigchunks_test.go`: the challenge bytes must be
  byte-identical with the flag on and off for today's short values, and a
  long synthetic value must round-trip through `readCLRVariant(_, true)`.
