# Dump upload: `gs://` and `azblob://` are documented but not compiled in

## Goal

Make the documentation of `DBB_DUMP_UPLOAD_URL` match what the binary can do.

## Why

`CLAUDE.md` (the `DBB_DUMP_UPLOAD_URL` row) and
`website/docs/features/session-dumps.md` ("`gs://` and `azblob://` work too")
say Google Cloud Storage and Azure Blob are supported. `internal/dump/upload.go`
blank-imports only `gocloud.dev/blob/fileblob` and `gocloud.dev/blob/s3blob`,
so `blob.OpenBucket` fails on the other two schemes and dbbat refuses to start.

Found while designing the statement archive
(`specs/ideas/2026-11-17-01-archive-statements-to-object-storage.md`), which
opens its bucket the same way.

## Implementation

Pick one:

- **Fix the docs** (recommended): say `s3://` and `file://` only, in
  `CLAUDE.md`, `website/docs/features/session-dumps.md` and the comment at
  `internal/config/config.go` around line 904. No new dependency.
- **Add the drivers**: blank-import `gocloud.dev/blob/gcsblob` and
  `gocloud.dev/blob/azureblob` in `internal/dump/upload.go`. This pulls the
  Google and Azure SDKs into the binary, so check the size before choosing it.

Either way, add a test that opens one URL per documented scheme, so the list
cannot drift again.
