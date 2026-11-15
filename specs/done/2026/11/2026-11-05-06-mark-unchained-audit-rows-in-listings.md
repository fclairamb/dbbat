# Mark unchained audit and query rows in API listings and the UI

## Goal

`GET /api/v1/audit`, the connection's query list and their UI pages flag a row
that carries no chain position (`chain_seq IS NULL`), so a reader sees which
entries nothing vouches for without running `dbbat audit verify`.

## Why

Follow-up of `2026-10-30-15-audit-verify-unchained-rows` (security scan
2026-10-30, finding L4). Verification now reports an unchained row written after
the chain began as a break, but the listings still show every row alike: a
forged entry backdated before `chain_seq 1` reads as a genuine pre-anchor row,
and between two verification runs any forged row looks genuine in the UI.

## Implementation

- `internal/store/models.go`: `AuditLog.ChainSeq` / `Query.ChainSeq` are
  `json:"-"`. Expose a derived boolean (`chained`) on the API response types
  rather than the integrity columns themselves.
- `internal/api/openapi.yml`: document the field on the audit and query schemas.
- `front/`: a small "unverified" badge (with `data-testid`) on rows where
  `chained` is false, with a tooltip pointing at `dbbat audit verify`.
- Tests: API test asserting the flag on a row inserted with a NULL `chain_seq`;
  an E2E check of the badge.
