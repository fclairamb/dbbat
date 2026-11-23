---
model: opus
effort: high
pr: batch
automerge: false
---

# The row-chain verifier excuses any connection whose unsealed `archived_at` is set

## Problem

Found by the security scan of 2026-11-23 (finding M2). Severity: Medium. Someone with write access to dbbat's own PostgreSQL store but without `DBB_KEY` can trigger it. That is the adversary the HMAC chains are designed to detect (see `docs/audit-chain.md`). It's a regression from the rows archive, `9a3608b` (v0.33.0).

`dbbat audit verify --rows` (and `GET /api/v1/audit/verify/rows`) walks every captured row chain, except the ones it leaves out here:

- `internal/store/chain_verify.go:1218-1220`: `capturedQueryUIDs` drops every query whose connection has `archived_at IS NOT NULL`.
- `chain_verify.go:1236-1255`: `countArchivedCaptures` counts those as archived or archive-deleted, and never walks them.

`archived_at`, `archive_key` and `archive_rows_sha256` are plain columns on `connections`, which carries no MAC. The archiver does write a chained `connection.archived` entry with the key, size, row count and SHA-256 (`internal/store/archive.go:162-173`), and a `connection.archive_deleted` one on deletion (`archive.go:318`). But the verifier never checks that such an entry exists, or that it matches the row.

So setting `archived_at` on a past connection, with no key involved, lets the actor delete or rewrite every captured result row of that session. `--rows` then reports it as archived and exits 0. Before the archive feature, the same tampering was a break: deleting rows of a stamped capture is caught, and wholesale deletion is caught through the sealed `query.rows_sealed` copies.

The same unsealed column also feeds the hash checks on reads. `GetConnectionArchive` (`archive.go:353-374`) takes the SHA-256 from `connections.archive_rows_sha256`, and `archive.Download` skips the check when it is empty (`internal/archive/reader.go:216-217`). An actor who can also write the bucket can swap the object and the column together.

## Proposal

1. In the row walk, excuse a connection's captures as archived only when a chained `connection.archived` entry exists for that connection UID. Its key, SHA-256, size and row count must equal the row's columns. Likewise, count it as archive-deleted only with a matching `connection.archive_deleted` entry. Otherwise, judge the capture as if `archived_at` were NULL, so missing rows are a break. One query joining `audit_log` on `details->>'connection_uid'` (check the `ArchiveSeal` JSON field names) keeps the walk at a fixed number of queries.
2. Report a mismatch between the entry and the row (key or hash differ) as a break of its own (`archive seal does not match the connection row`).
3. Take the SHA-256 for `archive.Download` and the paging reader from the sealed entry, not from the column, or require both to agree. Treat an empty or missing hash as `ErrHashMismatch`, never as "skip the check".
4. Update `docs/audit-chain.md` and `docs/archive.md`: the archive excuse is only as strong as its seal.

## Tests

- `internal/store` (testcontainers): archive a connection normally, and `--rows` verifies OK with `chains_archived = 1`. This is the positive control.
- Set `archived_at` on a non-archived connection with stamped captures (no `connection.archived` entry) and delete its `query_rows`. Verification reports a break. It fails before the change, which reports OK.
- Archive normally, then change `archive_rows_sha256` (or `archive_key`) on the row: a break.
- `internal/archive`: `Download` with an empty `SHA256` returns `ErrHashMismatch`. It fails before the change.

## To verify

- The exact JSON field names `ArchiveSeal` marshals to (`internal/store/archive.go`), and whether `connection.archive_deleted` carries the connection UID.
- Whether connections archived before an upgrade could lack the entry. They can't if the entry has always been written before the column, as `SealAndCommitArchive` does today.
