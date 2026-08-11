# `observeBigClrChunksFlag` reads the wrong capability byte

## Goal

Make `observeBigClrChunksFlag` (`internal/proxy/oracle/relay_preauth.go`) read
`ServerCompileTimeCaps[37]` — the byte every Oracle client reads
`UseBigClrChunks` from — instead of the byte four positions later, and decide
what the corrected value does to the AUTH paths that consume
`session.clientBigClrChunks`.

## Why

Found while writing the OER encoder for
`2026-08-10-17-oracle-refusal-frame-hangs-the-client`, which needed
`ServerCompileTimeCaps[15]` and `[16]` and so had to establish exactly where the
array starts.

Both `observeCustomHashFlag` and `observeBigClrChunksFlag` anchor on the byte
run `06 01 01 01` and then treat the array as starting **after** it. It does
not: those four bytes are `ServerCompileTimeCaps[0..3]`. Measured against
Oracle 19c and Oracle 23ai Free by parsing the Set Protocol reply the way
go-ora's `TCPNego.read` does, the length byte immediately preceding
`06 01 01 01` is the capability count (0x2a on 19c, 0x36 on 23ai) and the array
*includes* the anchor.

`observeCustomHashFlag` happens to be right anyway — it reads offset 0 of the
shifted array, which is the real `caps[4]`, and `caps[4]&0x20` is exactly what
go-ora's `auth_object.go` reads customHash from. `observeBigClrChunksFlag` is
not: it reads offset 37 of the shifted array, i.e. the real `caps[41]`.

On both servers measured:

| index | 19c | 23ai |
|---|---|---|
| real `caps[37]` (what clients read) | `0x7f` → `&0x20` set → **true** | `0x7f` → **true** |
| `caps[41]` (what dbbat reads) | `0x0d` → **false** | `0x0d` → **false** |

So dbbat concludes `UseBigClrChunks = false` on every session where go-ora,
JDBC thin and python-oracledb all conclude `true`.

`clientBigClrChunks` only feeds the *fallback* Phase 2 rewrite
(`rewritePhase2KVPairs`) and the CLR variant reader — the primary anchored
rewrite never decodes long values — which is presumably why nothing has broken
visibly. But it means the hardening that flag exists for (a long
`AUTH_CONNECT_STRING`, e.g. a load-balancer DNS host) has never actually been
exercised in the mode it was written for.

No GitHub issue yet — file one when picking this up.

## Implementation

- Reuse `serverCompileTimeCaps` from `internal/proxy/oracle/ttc_oer_encode.go`,
  which already parses the Set Protocol reply preamble properly (protocol
  version, server string, charsets, `numArray`, then the length-prefixed
  compile-time and runtime capability arrays). Point both `observeCustomHashFlag`
  and `observeBigClrChunksFlag` at it and index the array as Oracle does —
  `caps[4]&0x20` for customHash, `caps[37]&0x20` for big CLR chunks — so the two
  readers cannot drift apart again.
- Keep `observeCustomHashFlag`'s *value* unchanged (`caps[4]`); this is a
  refactor for it, not a fix. The auth suites
  (`upstream_auth_crypto_test.go`, `o5logon_test.go`) are the regression net.
- Then exercise the corrected `true` path: `clr_bigchunks_test.go` covers
  `readCLRVariant`, but there is no test that a long `AUTH_CONNECT_STRING`
  survives the Phase 2 fallback rewrite with the flag set. Add one, and run
  `make test-e2e-oracle` — the flag now flips on for every 19c/23ai session, so
  a latent bug in that path becomes a live one.
