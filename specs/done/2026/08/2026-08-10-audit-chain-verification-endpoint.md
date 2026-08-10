# Expose chain verification over the API

## Goal

Add an admin-only `GET /api/v1/audit/verify` (and a query-chain variant)
returning the same result as `dbbat audit verify`, so an auditor's evidence
script does not need shell access to the host.

## Why

`2026-08-09-tamper-evident-audit-chain.md` listed this as explicitly optional
and it was left out of the initial implementation. Today verification is
CLI-only, which is the right *default* — the endpoint must never leak the chain
key, and the CLI already runs where the key lives — but every other piece of
audit evidence on `website/docs/compliance.md` is collectable over the REST API,
and this one breaks that pattern.

Note the shape of the trade-off before implementing: an API that reports
"verified" is only as trustworthy as the process serving it, whereas the CLI can
be run by someone who does not trust the running server. Say so in the docs
rather than presenting the endpoint as equivalent.

No GitHub issue yet — file one when picking this up.

## Implementation

- Handler in `internal/api/`, admin role only, calling
  `Store.VerifyAuditChain` / `Store.VerifyQueryChains`
  (`internal/store/chain_verify.go`) — both already return structured results
  (`AuditChainResult`, `QueryChainsResult`, `ChainBreak`).
- Response must include the head MAC (hex) and the pre-anchor unverifiable
  count, and must never include the chain key or any payload bytes.
- A full walk is O(rows). Bound it: either a `?limit=` / `?since_seq=` window,
  or run it out of band and cache the last result. Do not let an unauthenticated
  burst turn into a table scan per request.
- `internal/api/openapi.yml` needs the route (there is a parity test), and
  `front/` needs `bun run generate-client` with the regenerated
  `src/api/schema.ts` committed.
- Cross-link from `website/docs/features/audit-chain.md` and the "Using this in
  an audit" section of `website/docs/compliance.md`.
