# Surface chain verification in the admin UI

## Goal

Add a "Verify chain" action to the audit page in `front/`, calling the
admin-only `GET /api/v1/audit/verify` and `GET /api/v1/audit/verify/queries`,
and show the outcome: verified / broken, the entry count, the head MAC (with a
copy button, because the operator is meant to record it outside the database),
the pre-anchor unverifiable count, and the break when there is one.

## Why

`2026-08-10-audit-chain-verification-endpoint.md` shipped the endpoints and
regenerated `front/src/api/schema.ts`, but no UI consumes them — verification
is still something you reach for with `curl` or the CLI. The head MAC in
particular is a per-quarter ritual (record it, compare it against last
quarter's), and a copy button is the difference between that ritual happening
and not happening.

Deliberately left out of the endpoint spec to keep it reviewable.

## Implementation

- New card or panel on `front/src/routes/_authenticated/audit/`, admin-only
  (hide it for viewers — the endpoints answer 403 and the UI should not offer a
  button that cannot work).
- `apiClient.GET("/api/v1/audit/verify")` — types already generated as
  `AuditChainVerification` / `QueryChainVerification` / `ChainBreak`.
- Render `cached` and `checked_at` honestly: the backend caches a walk for a
  minute, so a "Verify" click can legitimately return a result computed 50
  seconds ago. Do not present a cached answer as a fresh one.
- Carry the trust caveat into the UI copy, in one line, linking to
  `website/docs/features/audit-chain.md`: this answer comes from the server
  being audited, and `dbbat audit verify` is what someone who does not trust
  that server runs. Overselling it is the failure mode.
- A broken chain should be loud — error styling, the break's `chain_seq`, `uid`
  and reason shown verbatim.
- Add `data-testid` attributes and an e2e assertion in `front/e2e/`
  (`observability.spec.ts` covers the audit page today).

No GitHub issue yet — file one when picking this up.
