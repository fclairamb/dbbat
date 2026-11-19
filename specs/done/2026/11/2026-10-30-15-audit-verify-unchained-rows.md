---
model: opus
effort: medium
---

# Audit chain: rows written outside the chain after the anchor pass verification

## Problem

Found by the security scan of 2026-10-30 (finding L4).

Severity: Low. Needs write access to the storage database, not the key.

`VerifyAuditChain` counts `audit_log` rows with `chain_seq IS NULL` as `Unchained` and reports them as rows that predate the anchor (`internal/store/chain_verify.go:187-195`). It never checks that they actually do. The query-chain walk filters on `chain_seq >= ?` (`chain_verify.go:383-389`) and does not count NULL rows at all. So someone with write access to the store can insert a forged audit event (a grant approval, a login) or a forged statement into any session, with `chain_seq` NULL. The UI and `GET /api/v1/audit` show it as genuine, and `dbbat audit verify` reports the chain intact (the query walk without even a count).

## Proposal

1. Record when the chain started. The anchor row's `created_at` is under the same attacker's control, so prefer a value they cannot move without breaking a MAC: the anchor's sealed timestamp, or the first chained entry's.
2. In `VerifyAuditChain`, report an unchained `audit_log` row created after that point as a break, or at least as its own non-zero counter ("unchained after the anchor") that the CLI treats as a failure.
3. In the query-chain walk, count unchained `queries` rows of a session (`chain_seq IS NULL`) and do the same.
4. Optionally have `ListAuditEvents` / the UI mark unchained rows.

## Tests

- `internal/store/chain_test.go`: an `audit_log` row inserted with a NULL `chain_seq` and a current `created_at` makes `VerifyAuditChain` fail. A `queries` row inserted with a NULL `chain_seq` into a chained session makes the query walk fail. Both fail before the fix.
- Positive control: a store with genuinely pre-anchor rows (written before the chain migration) still verifies, with those rows counted as today.
