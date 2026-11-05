---
model: opus
effort: low
---

# SQL Server: approval patterns are matched against RPC text truncated to 8 KB

## Problem

Found by the security scan of 2026-10-30 (finding M7).

Severity: Medium. Triggered by any authenticated user on a SQL Server server whose grant definition has approval patterns (`DBB_APPROVAL_ENABLED`).

For an RPC, `describeRPC` joins the statements and truncates the result to `maxSQLTextLen` (8 KB) into `st.text` (`internal/proxy/mssql/intercept.go:126`, `truncateSQL` at `intercept.go:734`). `holdIfNeeded` then matches the approval patterns against that truncated `st.text` (`intercept.go:463`). The statement actually forwarded is the full one: grant enforcement runs on the untruncated `st.enforce`.

So an `sp_executesql` (or any statement-carrying RPC) whose matching part sits past the first 8 KB is never held. The four-eyes hold is skipped and the statement runs. SQLBatch text is not truncated before matching, and the other proxies match the full statement text, so only the SQL Server RPC path is affected.

## Proposal

1. In `holdIfNeeded`, match the patterns against the untruncated statement texts. Either match each entry of `st.enforce` (which also gives a statement-anchored pattern a chance on each statement), or keep an untruncated joined text next to the truncated one used for storage.
2. Keep the truncation only for what is stored and shown (`HoldRequest.SQL`, the `queries` row).

## Tests

- `internal/proxy/mssql` approval tests (`approval_test.go` or alongside `intercept_test.go`): an RPC whose statement is padded past 8 KB, with the matching keyword after the padding, is held. This fails before the fix.
- Positive control: a short RPC that does not match is not held, and a SQLBatch match keeps working.
