# Expose the captured-row chain verification over the API

## Goal

Add `GET /api/v1/audit/verify/rows` next to the two chain verification
endpoints that already exist, so the result-row chain is checkable the same way
the audit and query chains are.

## Why

`2026-08-10-chain-captured-result-rows.md` sealed `query_rows` and wired
`dbbat audit verify --rows`, but deliberately stopped short of the REST surface:
adding a third endpoint means an OpenAPI entry, a regenerated frontend client
and its own cache/role tests, which is a separate change from the chain itself.

Today the row chain is CLI-only, and both `docs/audit-chain.md` and
`website/docs/features/audit-chain.md` say so in as many words. That is honest
but asymmetric: an evidence script that collects `/audit/verify` and
`/audit/verify/queries` has to shell into the host for the third one.

No GitHub issue yet — file one when picking this up.

## Implementation

- `internal/api/audit_verify.go` already has the shape to copy: admin-only,
  `chainVerifyTTL` cache keyed per scope, at most one walk at a time, a `200`
  with `"verified": false` and a `break` object on a broken chain, and a
  response that carries counts, positions, the head MAC and the break reason but
  never the key or a record's content.
- The walker is `Store.VerifyRowChains(ctx, connectionUID *uuid.UUID)`
  (`internal/store/chain_verify.go`), returning `RowChainsResult`
  (`Captures`, `Verified`, `Unchained`, `Break`). Support `?connection=<uid>`
  the way the queries endpoint does. Consider whether `?query=<uid>` is worth it
  — `Store.VerifyRowChain` walks a single capture already.
- Add the path to `internal/api/openapi.yml` and regenerate the frontend API
  client (`front/`), matching what the queries endpoint did.
- Keep the caveat: the endpoint is served by the process under audit, and its
  answer can be up to `chainVerifyTTL` old. Then drop the "CLI-only for now"
  sentences from `docs/audit-chain.md`, `website/docs/features/audit-chain.md`
  and `website/docs/compliance.md`.
- Tests: mirror the existing chain-verification API tests — role gate, break
  reporting, and the non-leakage assertion pinning the response field set.
