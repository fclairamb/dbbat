# The wide/OCI AUTH leg ignores `UseBigClrChunks` on an unverified premise

## Goal

Either verify that an OCI client's AUTH KV framing really is independent of the
negotiated `UseBigClrChunks` capability, or thread the flag through the wide leg
the way `2026-08-12-01` just threaded it through the thin one.

## Why

`2026-08-12-01-oracle-ttckeyval-ignores-bigclrchunks` (archived) fixed the thin
path: `ttcClrVariant` / `ttcKeyValChunked` now write chunk lengths as
`ttcCompressedUint` when the session negotiated big chunks, and
`replaceAuthKVValue` reads with `readCLRVariant`. The **wide/OCI leg was left
out on purpose** and is disclosed in comments — but the reason it is safe is
weaker than the comments imply, which the completeness audit of that spec
caught:

- `readAuthKVPairWide` (`internal/proxy/oracle/upstream_auth_client.go`) and
  `replaceAuthKVValueWide` (`internal/proxy/oracle/phase2_forward.go`) take no
  `bigChunks` parameter at all and unconditionally use `readCLR` / `ttcClr`.
- The stated justification — "bigChunks is ignored in the wide path (OCI does
  not use it)", `upstream_auth_client.go` — is a **longstanding asserted claim**
  (it predates the 2026-08-12 fix, authored 2026-07-12), not something measured
  against an OCI capture or Oracle protocol documentation in this repo.
- Crucially, `s.clientBigClrChunks` is set from the **upstream server's**
  advertised capability (`observeBigClrChunksFlag` / `serverCapBitSet`,
  `relay_preauth.go`), not from anything client-specific. So it is true for
  essentially every 19c/23ai session *regardless* of whether the connecting
  client uses wide (OCI/sqlplus) or thin (go-ora, JDBC, python-oracledb)
  framing. The wide leg therefore genuinely **can** be reached with the flag
  set.

What keeps it safe today is not that the wide leg is unreachable — it is the
same "every substituted value is short" argument that made the thin-path bug
unreachable: `AUTH_SESSKEY` (64), `AUTH_PASSWORD` (96) and
`AUTH_PBKDF2_SPEEDY_KEY` (160) are all under the 252-byte short-form limit,
where both encodings are byte-identical. That argument expires the moment a
verifier or key size grows — exactly the reason `2026-08-12-01` was worth doing.

## Implementation

- **Verify the premise first**, because it decides whether there is any work at
  all. `testdata/` already holds a captured sqlplus login (used by
  `upstream_auth_client_wide_test.go`); check whether the capture's session
  negotiated `UseBigClrChunks`, and whether its AUTH KV values use single-byte
  or compressed-int chunk lengths. A capture with the capability set and
  single-byte chunks confirms the claim and closes this todo as
  documentation-only.
- If the claim holds, **replace the assertion with the evidence**: cite the
  capture in the comments on `readAuthKVPairWide` / `replaceAuthKVValueWide`,
  and say that the invariant is measured rather than assumed. Note explicitly
  that `clientBigClrChunks` being true does not imply the wide leg uses big
  chunks, since that is the counter-intuitive part.
- If it does **not** hold, thread `bigChunks` through the wide leg exactly as
  `2026-08-12-01` did for the thin one — `ttcClrVariant` on the write side,
  `readCLRVariant` on the read side — and add the same pair of tests: a
  round-trip over a >252-byte value, and a no-op regression proving short
  values are byte-identical under both flag values.
- Either way, cross-reference `specs/todos/2026-08-12-07-oracle-auth-challenge-ignores-bigclrchunks.md`,
  which is the third instance of this same latent bug (the client-facing
  `buildAuthChallenge`). All three should end up telling one consistent story.

No GitHub issue yet — file one when picking this up.

## Key files

- `internal/proxy/oracle/upstream_auth_client.go` (`readAuthKVPairWide`)
- `internal/proxy/oracle/phase2_forward.go` (`replaceAuthKVValueWide`)
- `internal/proxy/oracle/clr_bigchunks.go` (`ttcClrVariant`, the thin-path fix)
- `internal/proxy/oracle/relay_preauth.go` (`observeBigClrChunksFlag`)
- `internal/proxy/oracle/upstream_auth_client_wide_test.go`, `testdata/`
