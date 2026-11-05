---
model: opus
effort: medium
---

# PostgreSQL: max_query_counts is not enforced across pipelined Executes

## Problem

Found by the security scan of 2026-10-30 (finding L2).

Severity: Low. Triggered by an authenticated user whose grant has `max_query_counts`.

`checkQuotas` compares `grant.QueryCount` with the cap (`internal/proxy/postgresql/auth.go:254`). It runs for each `Execute` (`handleExecute`, `internal/proxy/postgresql/intercept.go:388`), but a successful statement only increments `QueryCount` when it completes, in `logQuery` at `ReadyForQuery` (`intercept.go:615` onwards). A client that pipelines N `Execute`s before one `Sync` passes N checks against the same stale count, so all N run. Repeated batch after batch, the cap only lands after the first batch that crosses it, whatever its size.

## Proposal

1. Charge the quota at admission: increment the in-session count when an `Execute` (or a simple `Query`) is forwarded, and stop incrementing it on completion. Refused statements are already counted at refusal (`recordBlockedQuery`).
2. Keep the persisted count, which drives the cross-session total, consistent with the in-session one.

## Tests

- Unit (`intercept_test.go`): with `max_query_counts = 2`, a third `Execute` queued before any completion is refused with `ErrQueryLimitExceeded`. This fails before the fix.
- Positive control: two pipelined Executes under a cap of 2 both run, and sequential queries keep the current behavior.

## To verify

- How the cross-session count is computed (`populateGrantCounters`) and whether admission-time counting double-counts with it.
