# Chain the captured result rows

## Goal

Extend the tamper-evident chain to `query_rows`, so the captured result set of
a query is as tamper-evident as the statement that produced it.

## Why

`2026-08-09-tamper-evident-audit-chain.md` shipped HMAC chains over `audit_log`
(one global chain) and `queries` (one chain per connection). `query_rows` — the
optional capture of the rows a statement actually returned, bounded by
`max_result_rows` / `max_result_bytes` — is **not** covered. Anyone with write
access to the storage database can alter or delete captured rows undetectably,
which is precisely the exfiltration evidence an investigation would lean on.

The compliance page now cites the chain for ISO/IEC 27001:2022 A.8.15 and
PCI DSS v4.0 requirement 10.3. Leaving the row capture unsealed is a real edge
in that story and should either be closed or stated explicitly on the page.

No GitHub issue yet — file one when picking this up.

## Implementation

- The natural shape is a chain **per query**, mirroring the per-connection
  choice for `queries`: retention deletes whole queries (and cascades to their
  rows), so a per-query chain is severed exactly when its parent goes away.
  `row_number` is already a dense, ordered position — use it as the chain
  position rather than adding a `chain_seq`.
- Migration: `prev_mac`/`mac` on `query_rows`, plus a `row_chain_mac` /
  `row_chain_len` stamp on `queries` written when the capture finishes, so a
  trailing deletion is detectable (same trick as `connections.query_chain_mac`).
- The write path is `Store.StoreQueryRows` (`internal/store/queries.go`), fed by
  the batched writer in `internal/proxy/shared/rowwriter.go`. Unlike the query
  chain, a single batch spans several queries and therefore several chains, so
  the head cache has to be keyed by query uid and the batch has to be split per
  query before MACs are computed. Watch the cost: this is the hottest write path
  in the store, and it currently amortizes one INSERT across many rows.
- `row_data` is `jsonb`, so reuse `canonicalJSON` from
  `internal/store/chain_canonical.go` — the same fixed-point argument applies.
- Extend `dbbat audit verify --queries` (or add `--rows`) and
  `internal/store/chain_verify.go`.
- Decide deliberately whether `results_truncated` / `results_dropped` belong in
  the MAC: they are written after the fact, like the query outcome columns, and
  the same "cannot re-seal without invalidating successors" argument applies.
- Update `docs/audit-chain.md`, `website/docs/features/audit-chain.md` and the
  scope caveats in `website/docs/compliance.md`.
