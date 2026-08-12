# `ttcKeyVal` re-encodes AUTH values with single-byte CLR chunks even when UseBigClrChunks is on

## Goal

Make the Phase 2 rewrite *write* long CLR values in whichever encoding the
session negotiated, instead of always writing `ttcClr`'s single-byte chunk
lengths.

## Why

Found while fixing `2026-08-11-08-oracle-bigclrchunks-cap-offset`, which turned
`session.clientBigClrChunks` on for every real 19c/23ai session for the first
time.

The rewrite is now correctly *reading* a client's big-chunk-encoded values
(`readCLRVariant(buf, bigChunks)`), but both write paths still go through
`ttcKeyVal` → `ttcClr`, which emits the 0xFE long form with **single-byte**
chunk lengths:

- `rewritePhase2KVPairs` (the fallback KV walk), `internal/proxy/oracle/phase2_forward.go`
- `replaceAuthKVValue` (the anchored splice), same file

A value of 252 bytes or more written that way is parsed by an upstream that
negotiated `UseBigClrChunks` as a compressed-int chunk length — `0xFC` reads as
"a 0-byte length field" rather than "252 bytes follow" — and the AUTH message
desyncs.

Today this is **unreachable**, which is why it is a todo and not part of the
fix: the only values dbbat substitutes are `AUTH_SESSKEY` (64 hex chars),
`AUTH_PASSWORD` (96) and `AUTH_PBKDF2_SPEEDY_KEY` (160), all comfortably under
the 252-byte short-form limit, and short values are byte-identical in both
encodings. It becomes reachable the moment a verifier or key size grows, or
another (longer) value is ever substituted.

No GitHub issue yet — file one when picking this up.

## Implementation

- Give `ttcClr` a big-chunk sibling (or a `bigChunks bool` parameter) that
  writes chunk lengths as `ttcCompressedUint` — the encoder counterpart of
  `readCLRVariant`. The test helper `encodeBigChunkCLRSplit` in
  `internal/proxy/oracle/clr_bigchunks_test.go` already is that encoder; move it
  into the package proper.
- Thread the flag into `ttcKeyVal` and into `replaceAuthKVValue` /
  `rewriteAuthPhase2Anchored`, which currently do not receive it at all
  (`rewriteAuthPhase2` has it and only forwards it to the fallback walk).
- Add a round-trip test: encode a >252-byte value with `bigChunks=true`, rewrite
  it, and read it back with `readCLRVariant(_, true)`. The existing
  `TestRewritePhase2KVPairs_BigChunkConnectString` covers the read side and can
  be extended with a substituted long value on the write side.
- `replaceAuthKVValue` also reads the value it is replacing with plain `readCLR`
  rather than `readCLRVariant`; that has the same "only short values today"
  argument and should be fixed in the same pass.
