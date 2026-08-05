---
model: opus
effort: high
---

# Session captures should be storable in S3, not only on local disk

## Problem

Session captures are written straight to local disk and nowhere else:

- Each proxy builds `<DBB_DUMP_DIR>/<connectionUID>.pcapng` and hands the path
  to `dump.NewWriter` (`internal/proxy/postgresql/session.go:272`,
  `internal/proxy/oracle/session.go:367`, `internal/proxy/mysql/session.go:341`,
  `internal/proxy/mongodb/session.go:581`).
- `dump.Writer` owns an `*os.File` directly (`internal/dump/writer.go:56`).
- Retention is a local `os.ReadDir` + `os.Remove` sweep
  (`internal/dump/cleanup.go:20`), started independently by all four proxy
  servers on the same directory.
- The API serves a capture by joining the same path again
  (`internal/api/observability.go:309`).

On Kubernetes this means captures live on ephemeral pod storage: they die with
the pod, they are invisible to replicas (`DBB_INSTANCE_ID` deployments), and
`DBB_DUMP_MAX_SIZE` / `DBB_DUMP_RETENTION` exist mostly because local disk is
scarce. S3 (or any blob store) is the natural durable home for finished
captures.

Original ask: a generic VFS / Reader / Writer so streams can go either to local
disk or to an S3 repo, with a layout like `s3://$repo/$serverId/$connectionId.ngcap`.

## Proposal

**Recommended shape: keep writing locally, upload the finished file to blob
storage on session close ("spool and upload"), and serve reads through a
storage abstraction.** Do not stream pcapng bytes to S3 while the session is
live:

- S3 objects are immutable — no append. Live streaming forces multipart upload
  (≥5 MiB parts), loses the flush-per-packet behaviour the writer has today,
  and a crash mid-session strands an invisible incomplete multipart upload.
- The current design already gives a valid partial pcapng on crash because the
  file is flushed incrementally; spooling preserves that.

Concretely:

1. **Storage abstraction**: use `gocloud.dev/blob` (Go CDK) rather than
   hand-rolling a VFS. It is exactly the asked-for generic Reader/Writer with
   `file://`, `s3://` (and `gs://`, `azblob://` for free) behind one API, and
   it handles credentials via the standard AWS chain. If we prefer zero new
   heavyweight deps, a narrow internal interface
   (`Put(ctx, key, io.Reader)`, `Open(ctx, key) (io.ReadCloser, error)`,
   `Delete`, `List`) with a local and an S3 (`aws-sdk-go-v2`) implementation
   is acceptable — but don't build a general filesystem.
2. **Write path**: `dump.Writer` keeps writing to the local spool
   (`DBB_DUMP_DIR`, unchanged). On `Close()` (all four `session.go` call
   sites), if remote storage is configured, enqueue an upload of the finished
   file, then delete the local copy on success. Uploads must survive transient
   S3 failures (retry queue; on startup, sweep the spool for finished-but-not-
   uploaded files, which also covers crash recovery).
3. **Key layout**: `s3://$bucket/$prefix/$instanceID/$connectionUID.pcapng`
   (`$serverId` from the ask = `DBB_INSTANCE_ID`). Keep the `.pcapng`
   extension — it is a real, tool-readable format (`internal/dump/dump.go:10`);
   `.ngcap` would only obscure that. **Store the resulting object key (or a
   `dump_location` enum + key) on the `connections` row**: the API looks up
   captures by connection UID alone (`internal/api/observability.go:309`) and
   cannot know which instance wrote the file, so without a stored key every
   read becomes a LIST. A stored key also gives the UI a cheap "capture
   exists" signal. Consider a date segment (`$prefix/$YYYY/MM/$instanceID/…`)
   for human browsing; with the key stored on the row, the layout is free to
   change later.
4. **Read path**: the two API handlers (`internal/api/observability.go:309`,
   `:338`) resolve local-first, then remote via the stored key, streaming the
   object body through. `dbbat dump anonymise` stays a local-file tool; anyone
   can download first. (Optional later: anonymise-on-upload hook.)
5. **Retention**: `DBB_DUMP_RETENTION` applies to the local spool only and is
   **ignored for S3** — remote retention is the bucket's lifecycle policy,
   which the deployment docs must state explicitly. Never replicate the sweep
   with LIST+DELETE calls against S3 (today's per-proxy `CleanupOldFiles`
   goroutines in `internal/proxy/*/server.go` would 4× the LIST cost; they
   keep sweeping only the local spool).
6. **Config**: add `DBB_DUMP_UPLOAD_URL` (e.g. `s3://bucket/prefix`; empty =
   current local-only behaviour). Keep `DBB_DUMP_DIR` as the spool. With
   gocloud.dev the URL scheme picks the driver, so `file:///…` works too and
   tests can use it.

### Decisions (settled 2026-08-05)

- `DBB_DUMP_RETENTION` is ignored for S3: remote retention is exclusively the
  bucket lifecycle policy. The app enforces retention only on the local spool.
- Upload happens **on session close**, one object per connection. If capture
  rotation is ever added (today `DBB_DUMP_MAX_SIZE` stops the capture rather
  than rotating), the uploader gets revisited then — out of scope here.
- Long-lived sessions keep their capture local until close; losing that
  capture if the pod dies mid-session is **accepted**. No checkpoint upload of
  still-open captures.

No GitHub issue filed yet — one should be created when this is picked up.

## Implementation Plan

1. **Dependency**: add `gocloud.dev/blob` with the `fileblob` and `s3blob`
   drivers (blank-imported). `file://` makes the whole upload path unit-testable
   with no cloud dependency.
2. **`internal/dump/upload.go`** — new `Uploader`:
   - `OpenUploader(ctx, UploaderOptions)`; `URL` empty ⇒ `nil, nil` (local-only,
     the default, unchanged behaviour). Every method is nil-receiver safe so the
     four proxies and the API can call it unconditionally.
   - URL handling: `s3://bucket/prefix` is rewritten to `s3://bucket?prefix=…`
     before `blob.OpenBucket` (the s3 opener only reads the host); `file://`
     and `mem://` pass through untouched.
   - Key layout `YYYY/MM/DD/<instanceID>/<connectionUID>.pcapng`, the date taken
     from the spool file's mtime so a retry recomputes the *same* key.
   - `Finish(ctx, uid)` enqueues onto a buffered channel drained by a small
     worker pool; each job does upload → record key on the connections row →
     delete the local spool file, with bounded retries. A failure leaves the
     spool file alone so the next startup sweep retries it.
   - `SweepSpool(ctx)` enqueues every `*.pcapng` left in the spool dir —
     crash recovery, run at startup before any proxy accepts.
   - `Open`/`Delete` for the API read/delete path; `Close` drains the queue.
3. **Store**: `dump_key` column on `connections` (migration
   `20260805120000_connections_dump_key.{up,down}.sql`), `Connection.DumpKey`
   field, `SetConnectionDumpKey` / `ClearConnectionDumpKey`, and `dump_key`
   added to the explicit column lists of `GetConnectionByUID` / `ListConnections`.
   `json:"-"` — the key is internal bookkeeping, not API surface.
4. **Config**: `DumpConfig.UploadURL` (`DBB_DUMP_UPLOAD_URL` via the existing
   `dump_` prefix rule). `Load` rejects an upload URL with no `DBB_DUMP_DIR`:
   the spool is what gets uploaded.
5. **Write path**: each proxy `Server` gains an `uploader` field + `SetUploader`
   (same shape as `SetRowWriter`); sessions call the nil-safe
   `uploader.Finish(ctx, uid)` right after `dumpWriter.Close()`. Nothing about
   `dump.NewWriter` or the local write changes.
6. **Read path**: `handleGetConnectionDump` serves the local spool first, then
   falls back to the stored key and streams the object body.
   `handleDeleteConnectionDump` removes both the local file and the object, and
   clears `dump_key`. The API server gets `SetDumpStorage`.
7. **Retention**: `CleanupOldFiles` is untouched and keeps sweeping the local
   spool only. Documented that S3 retention is the bucket lifecycle policy.
8. **Wiring**: `main.go` opens the uploader once, sweeps the spool, hands it to
   the API server and the four proxies, and closes it on shutdown.
9. **Docs**: `DBB_DUMP_UPLOAD_URL` in the root `CLAUDE.md` env table and a
   "Where captures are stored" section in `docs/dump-format.md`.
10. **Tests**: `internal/dump/upload_test.go` drives upload, read-through,
    delete, key layout, URL rewriting and the crash-recovery sweep against a
    `file://` bucket in a temp dir; `internal/config` covers the new env var and
    the validation error.
