---
model: sonnet
effort: medium
pr: batch
automerge: false
---

# PostgreSQL `block_copy` only matches `COPY` followed by a space

## Problem

Found by the security scan of 2026-11-23 (finding L1). Severity: Low. Any user with a PostgreSQL grant that carries `block_copy` and not `read_only` can trigger it.

`isCopyQuery` (`internal/proxy/postgresql/intercept.go:571-573`) is `shared.HasNormalizedSQLPrefix(sql, "COPY ")`, a byte prefix that requires a literal space (`internal/proxy/shared/sqlcomments.go:106-111`). PostgreSQL accepts any whitespace after the keyword, so a `COPY` written with a tab or a newline after the keyword is not classified as one. It reaches the server under a `block_copy` grant. The `CopyOutResponse` / `CopyInResponse` handling in `internal/proxy/postgresql/session.go:1562-1584` only tracks the copy; it doesn't refuse it. Reproduced at the validator level (`validateStatement` returns nil).

Low because what `block_copy` withholds (a bulk export to the client or a bulk load from it) is mostly available row by row through `SELECT` / `INSERT` on the same grant. `COPY … TO/FROM PROGRAM` or a server file needs upstream privileges the shared role should not have.

## Proposal

1. Make `isCopyQuery` match the keyword followed by any whitespace or the end of the text. Use the same word-boundary rule the other classifiers use, for example the pattern `(?i)^COPY\b` through `shared.MatchesNormalizedSQL` on the trimmed text. The batch walk (`anyStatement`) then covers every statement unchanged.
2. Grep the other `HasNormalizedSQLPrefix(…, "X ")` callers for the same space-only assumption and fix them the same way.
3. Defence in depth: refuse a `CopyInResponse` / `CopyOutResponse` from upstream on a `block_copy` session (send `CopyFail` or terminate). The server then can't start a copy dbbat's text check missed, whatever the spelling.

## Tests

- `internal/proxy/postgresql/intercept_test.go`: `COPY` followed by a tab, then by a newline, under `block_copy`: `ErrCopyNotPermitted`. Fails before the change.
- Positive controls: `COPY t TO STDOUT` is still refused; `SELECT 1 AS copy_count` and a table named `copy_log` are allowed.
- If step 3 is done: a session test where the upstream answers with `CopyOutResponse` on a `block_copy` grant ends the copy.
