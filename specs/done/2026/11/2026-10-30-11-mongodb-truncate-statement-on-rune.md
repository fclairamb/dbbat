---
model: opus
effort: low
---

# MongoDB: a statement cut mid-character at 8 KB is never logged

## Problem

Found by the security scan of 2026-10-30 (finding M11).

Severity: Medium. Any authenticated user on a MongoDB server can trigger it on purpose.

`buildSQLText` renders a command as its name plus relaxed Extended JSON, then cuts it at `maxSQLTextLen` bytes with `text[:maxSQLTextLen]` (`internal/proxy/mongodb/result.go:361-373`). Relaxed Extended JSON writes non-ASCII characters as raw UTF-8, so the cut can land inside a multi-byte character and leave invalid UTF-8. Nothing between this function and the store repairs it (`shared.SanitizeStatementText`, `internal/proxy/shared/errortext.go:78`, is not called on this path), and PostgreSQL refuses invalid UTF-8 in a `text` column. The `queries` insert then fails, and the command, which has already run upstream, is missing from the query history, the UI and the stream.

A user who pads a command so that a multi-byte character straddles byte 8192 makes any command they choose disappear from the log. The SQL Server proxy already cuts on a rune boundary (`truncateSQL`, `internal/proxy/mssql/intercept.go:734-744`).

## Proposal

1. In `buildSQLText`, cut on a rune boundary (walk back with `utf8.RuneStart`, as `truncateSQL` does), or share one helper in `internal/proxy/shared`.
2. As defense in depth, make sure every proxy's statement text goes through `shared.SanitizeStatementText` (or `strings.ToValidUTF8`) before `CreateQuery`, so no client-chosen bytes can make the insert fail.

## Tests

- `internal/proxy/mongodb/result_test.go`: a command whose Extended JSON puts a multi-byte character across `maxSQLTextLen` yields valid UTF-8 no longer than the limit. This fails before the fix.
- Positive control: ASCII text longer than the limit is still cut at exactly `maxSQLTextLen`, and short text is unchanged.

## To verify

- Whether a failed `CreateQuery` on MongoDB leaves any trace (a WARN only?), and whether other protocols truncate byte-wise anywhere else (`grep -rn '\[:maxSQLTextLen\]' internal/proxy`).
