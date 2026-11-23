---
model: opus
effort: medium
pr: batch
automerge: false
---

# Archived rows larger than 8 MiB are served without any integrity check

## Problem

Found by the security scan of 2026-11-23 (finding L3). Severity: Low. Someone with write access to the archive bucket (`DBB_ARCHIVE_URL`) but not to dbbat can trigger it.

`archive.open` (`internal/archive/reader.go:45-62`) fetches an object whole and checks its SHA-256 only up to `WholeObjectLimit` (8 MiB, `reader.go:19-23`). A larger object is opened over range requests with no check, which the comment states as a choice. That path serves `GET /api/v1/queries/{uid}/rows` for an archived capture (`internal/api/archive.go:105-106`), an admin-or-viewer route. The reader also trusts the Parquet file's own statement-to-offset metadata, without checking that the records it returns belong to the requested query.

So for exactly the busy sessions large enough to matter, someone who can rewrite a bucket object can change the captured result rows a viewer reads in the UI, after the PostgreSQL copy is gone. The SHA-256 sealed in the audit chain would catch that, but it is never consulted on this path. The raw download (`/connections/{uid}/archive/rows`) does check it.

## Proposal

1. Check integrity on the paging path without fetching the whole object. At seal time, write a per-chunk digest list (for example SHA-256 per Parquet row group) next to the whole-object hash, inside the sealed `connection.archived` entry. Verify each row group the reader touches against it. Spec `2026-11-23-02` makes the entry the source of truth for these hashes.
2. While paging, check that each returned record's `QueryUID` (and `ConnectionUID`) equals the requested one. Treat a mismatch as `ErrBadMetadata`.
3. Objects archived before the change, which have no chunk digests: either fetch whole and hash when under a higher ceiling (for example 64 MiB), or label the rows "unverified" in the response (`verified: false`) so the UI can say so. Never present them as verified.
4. Update `docs/archive.md`.

## Tests

- `internal/archive`: an object over the limit with one row group altered after sealing makes the paging read fail with `ErrHashMismatch`. Fails before the change.
- An object whose offset metadata points one query's range at another query's records makes the read fail with `ErrBadMetadata`.
- Positive control: an untouched large object pages correctly, and the small-object path is unchanged.

## Open questions

- Per-chunk digests in the seal (exact, more data in `audit_log`) or a higher whole-fetch ceiling (simpler, still unverified past it)? Recommended: chunk digests. The ceiling only moves the gap.
