# Archive statements to object storage, then delete them, per server

## Goal

Move a closed connection's statements and captured result rows out of
PostgreSQL into object storage (S3) as raw columnar files, and delete them
later. Two parameters drive it:

- `archive_after_days`: days before a connection's data leaves PostgreSQL for
  the bucket.
- `delete_after_days`: days before it is deleted for good.

Both resolve at three levels: the instance, a server group, a server.

## Why

Today retention is one instance-wide window (`DBB_QUERY_STORAGE_RETENTION`),
and the only choice is "keep it in PostgreSQL" or "delete it". That forces a
bad trade:

- `query_rows` is the bulk of the store. Keeping a year of it in PostgreSQL to
  satisfy an audit requirement makes the storage database large and slow for
  data nobody reads after the first week.
- A production server under a compliance rule and a staging server full of
  throwaway sessions get the same window.

An archive tier fixes the first, per-server windows fix the second.

## Decisions

### 1. Format: Parquet

| | Parquet | ORC | Vortex |
|---|---|---|---|
| Pure-Go writer (the build is `CGO_ENABLED=0`) | Yes, two maintained: `parquet-go/parquet-go` v0.32, `apache/arrow-go` v18.9 | No official Go library. `scritchley/orc` has no release and its README says not to use the writer yet | None. One third-party read-only reader, 0 importers. Writing means cgo over the Rust library |
| Readers | DuckDB, Athena, Trino, Spark, ClickHouse, BigQuery, Snowflake, pandas, polars | Hive, Trino, Spark | Rust, Python, Java, DuckDB core extension (Linux and macOS only) |
| Stability | Frozen for years | Frozen | File format stable since 0.36, crates still 0.x (0.86+), LF AI & Data incubation |
| Fit | zstd, dictionary, page index, bloom filters, column encryption | Same class | Faster random access and scans, which a write-once cold archive does not need |

Parquet is the only candidate dbbat can write without cgo, and the one every
query engine reads. If Vortex ever matters, converting is one DuckDB `COPY`.

Library: `github.com/parquet-go/parquet-go`. Struct-tag API, pure Go, no Arrow
dependency. `arrow-go` is the fallback if it stalls.

dbbat does not embed a query engine. DuckDB's Go client needs cgo.

### 2. Unit: one closed connection

One connection = one `queries` object and, when it captured rows, one `rows`
object. Only closed connections are archived.

Why this unit:

- The `queries` audit chain is already one chain per connection and
  `query_rows` one per query, so a file holds whole chains and stays verifiable
  on its own.
- S3 objects are immutable. Per-server deletion needs a unit that can be
  deleted without rewriting a shared file.
- It is the pattern `dump_key` already uses: write the object, record the key
  on the connection row, never LIST the bucket.
- One connection row is a natural claim for multi-replica safety.

The cost is many small files, which makes fleet-wide scans slower (one or two
GETs per file). dbbat connections are human and agent sessions, hundreds a day,
not application pools, so this is acceptable. A compaction job can come later
if it is not. It is not in this design.

### 3. Layout

```
<prefix>/v1/queries/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
<prefix>/v1/rows/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
```

- Table first, then hive partitions, so an engine can glob one table and prune
  on `date` and `server` from the path alone.
- `date` is the UTC date of `disconnected_at`. The key is a pure function of
  the connection row, so a retry overwrites the same object.
- `server` is the uid. Names are editable; the name at archival time is a
  column inside the file.
- `v1` is the schema version.

### 4. Schema

`queries`, one row per statement, sorted by `chain_seq`:

| Column | Type | Note |
|---|---|---|
| `connection_uid`, `user_uid`, `server_uid`, `grant_uid` | UUID | Connection identity, repeated on every row (dictionary-encoded, close to free) |
| `username`, `server_name`, `protocol`, `source_ip` | string | As of archival time |
| `connected_at`, `disconnected_at` | timestamp(µs, UTC) | |
| `query_uid` | UUID | UUIDv7, so min/max statistics prune by time |
| `chain_seq` | int64 | |
| `executed_at` | timestamp(µs, UTC) | |
| `duration_ms` | decimal(10,3) | Exact, as stored |
| `sql_text` | string | |
| `parameters` | JSON string | The canonical form the MAC is computed over |
| `rows_affected` | int64, optional | |
| `error`, `copy_format`, `copy_direction` | string, optional | |
| `results_truncated`, `results_dropped` | boolean | |
| `approval_status`, `approval_pattern`, `resolved_by`, `resolved_at`, `resolution_reason` | optional | |
| `prev_mac`, `mac`, `row_chain_mac` | binary | |
| `row_chain_len` | int64 | |

`rows`, one row per captured result row, sorted by statement order then
`row_number`:

| Column | Type | Note |
|---|---|---|
| `connection_uid`, `query_uid` | UUID | |
| `row_number` | int32 | |
| `row_data` | JSON string | Canonical form. Every statement has its own columns, so this stays one JSON column |
| `row_size_bytes` | int64 | |
| `prev_mac`, `mac` | binary | |

File key-value metadata on both: `dbbat.schema_version`, `dbbat.version`,
`dbbat.connection` (the connection row as JSON, including `query_chain_mac`,
`query_chain_len`, `query_chain_stamp_version`).

zstd everywhere. Row groups capped (50k rows) so the writer's memory is bounded
whatever the session size.

Everything a MAC covers is kept bit-exact (canonical JSON, microsecond UTC
timestamps, exact decimal), so the chains can be re-verified from the files.

VARIANT for `row_data` is not used: reader support is uneven and the MAC is
over the canonical JSON text anyway.

### 5. Policy and resolution

Storage:

- `servers.archive_after_days`, `servers.delete_after_days`: nullable integers.
- `server_groups`: the same two columns.
- Instance: store parameters `retention.archive_after_days` and
  `retention.delete_after_days`, which win over the environment, like
  `limits.statement_timeout`. Environment defaults: new
  `DBB_ARCHIVE_AFTER_DAYS` (default: never), and the existing
  `DBB_QUERY_STORAGE_RETENTION` for deletion.

Three states per value, same convention as `statement_timeout_seconds`:
`NULL` inherits, `0` means never (and overrides what is below), a positive
number is days.

Each parameter resolves on its own, in one place
(`store.ResolveRetentionByServers`, shaped like
`ResolveServerApproverGroupsByServers`: two batched queries):

1. The server's value, when not `NULL`.
2. Otherwise the server groups that set one. When several do, **keep the
   longest**: deletion takes the largest window (never beats everything),
   archival takes the smallest (data moves to the bucket as soon as any group
   asks). Conflicting policies resolve to the least destructive outcome.
3. Otherwise the instance value.

After resolution, archival applies only when `0 < archive < delete` or delete
is never. Otherwise the data is simply deleted at its delete window.

Policy is read live at sweep time, never snapshotted. Editing a window, or
moving a server between groups, applies to data already stored. Lowering a
delete window destroys data at the next sweep: the UI warns at the point of
edit and the change is audited.

The resolver must read soft-deleted servers too, so their sessions keep the
window the server had.

`ssh` and `kubernetes` rows are dial paths with no sessions: no retention
fields for them.

### 6. Clocks

- **Archival**: `disconnected_at + archive_after_days`. Open sessions are never
  archived.
- **Deletion of statements still in PostgreSQL**: `executed_at +
  delete_after_days`, as today, but per resolved window (one batched delete per
  distinct window, joined through `connections.database_id`). A long-lived
  session keeps losing its oldest statements, which the verifier already
  accepts as a truncated prefix.
- **Deletion of an archive**: `disconnected_at + delete_after_days`. A file
  goes when its newest statement is past the window, so the oldest statements
  of a long session outlive the window by at most the session's length.
- **Ledger**: a connection row is never deleted before its statements. The
  connection sweep uses `max(connection window, resolved delete window)` and
  skips any row with a live archive. Without this the object key is lost and
  the object orphaned.

Delete is a hard deadline. If the bucket is unreachable for longer than
`delete - archive` days, the data is deleted from PostgreSQL without having
been archived, with a WARN.

### 7. Bucket

- `DBB_ARCHIVE_URL`, a `gocloud.dev/blob` URL (`s3://bucket/prefix`,
  `file://`), opened with the same helper `internal/dump/upload.go` uses. It
  may be the dump bucket with another prefix.
- Unset: archival is inert everywhere, statements stay in PostgreSQL until
  their delete window, `GET /instance` reports `archive_enabled: false` and the
  UI disables the archive fields.
- Needs `PutObject`, `GetObject`, `DeleteObject`. No `ListBucket`.
- dbbat deletes archive objects itself, because a bucket lifecycle rule cannot
  express per-server windows. A lifecycle rule longer than every window is a
  fine backstop.
- Encryption is the bucket's (SSE-S3 or SSE-KMS). `row_data` is plaintext in
  PostgreSQL today, so this is not a regression. Parquet column encryption can
  be added later.

### 8. Archiver

New package `internal/archive`. It runs as phases of the existing hourly
retention sweep (`retention.go`), which gains jitter:

1. Archive due connections.
2. Delete due archives.
3. Delete due statements still in PostgreSQL.
4. Delete due ledger rows.

Per connection:

1. **Claim** with a conditional `UPDATE` on `connections.archive_claimed_at`
   (lease of 1h, `FOR UPDATE SKIP LOCKED` on the candidate select), the
   `ClaimGrantBytesSteps` pattern. Candidates: closed, due, not archived, with
   at least one statement.
2. **Write**: stream statements in `chain_seq` order from PostgreSQL through
   the Parquet writer into the blob writer, hashing with SHA-256 on the way.
   Same for rows. No local spool.
3. **Seal**: append a chained `connection.archived` audit entry with the keys,
   sizes, SHA-256 of each object, statement and row counts, and the policy
   that applied (days and which level it came from).
4. **Commit**, one transaction: set `archived_at`, `archive_key`,
   `archive_queries_sha256`, `archive_rows_sha256` on the connection row and
   `DELETE FROM queries WHERE connection_id = ?` (rows cascade).

A crash anywhere leaves the statements in PostgreSQL. The lease expires, the
next sweep redoes the connection, the objects are overwritten at the same key
and a second audit entry is appended. The latest `connection.archived` entry
is authoritative.

Archive deletion: delete both objects, append `connection.archive_deleted`
(chained), set `archive_deleted_at` and clear the key.

Both event types are excluded from an unfiltered `GET /api/v1/audit`, like
`connection.opened` and `connection.closed`.

### 9. Audit chain

The verifier stops inferring from one global window and reads evidence:

- **Archived** (`archived_at` set, statements gone from PostgreSQL): not a
  break. The row's SHA-256 values must equal the latest `connection.archived`
  entry. Counted as `chains_archived`. No bucket access.
- **Archived, full check**: `dbbat audit verify --queries --archive` (and
  `--rows --archive`) downloads each object, checks its SHA-256, and walks the
  chains inside the file against `query_chain_mac` and `row_chain_mac`. CLI
  only: it is O(archive).
- **Archive deleted**: excused only by a `connection.archive_deleted` entry.
  Counted as `chains_archive_deleted`.
- **Deleted from PostgreSQL without archive**: `StatementsPastRetention` takes
  the window resolved for the connection's server, with the same resolver the
  sweep uses. "Excused" keeps meaning "the sweep would have deleted it".

A tampered or missing object is therefore detectable without trusting the
bucket: its hash is sealed in the audit chain.

### 10. Reading

Inside dbbat (per connection, by key, with range reads through
`bucket.NewRangeReader`):

- `GET /connections/{uid}` gains `archived_at`, `archive_deleted_at` and
  `statements_location`: `store`, `archive` or `deleted`.
- `GET /queries?connection_id=` on an archived connection is served from the
  archive, same cursor.
- `GET /queries/{uid}` and `/queries/{uid}/rows` accept `?connection_uid=`. A
  statement uid alone cannot locate an archive without a LIST, so the frontend
  passes the connection it navigated from.
- `GET /connections/{uid}/archive/queries` and `/archive/rows` stream the raw
  Parquet objects, like the dump download.
- The unfiltered `/queries` list covers PostgreSQL only, and the page says so.

Outside dbbat, any engine pointed at the bucket:

```sql
SELECT username, server_name, count(*) AS statements, sum(duration_ms) AS total_ms
FROM read_parquet('s3://bucket/dbbat/v1/queries/**/*.parquet', hive_partitioning = true)
WHERE date >= '2026-10-01' AND sql_text ILIKE '%customers%'
GROUP BY ALL;
```

### 11. API and UI

- `PUT /instance/retention` with the two values (`null` falls back to the
  environment). `GET /instance` returns the raw values, the resolved ones with
  their source (`parameter` or `env`), and `archive_enabled`.
- `PUT /servers/{uid}` and `PATCH /server-groups/{uid}` take the two fields,
  tri-state in JSON: absent leaves it alone, `null` inherits, a number sets it.
- `GET /servers/{uid}` returns `effective_retention`: both resolved values and
  where each came from (`server`, `server_group` with the uids, `instance`).
- 400 on a negative value, or on `archive >= delete` set at the same level.
- Settings page: a Retention card. Server and server-group dialogs: two fields
  whose placeholder shows the inherited value and its source. Connection page:
  an Archived badge, statements loaded from the archive, download buttons, and
  a "deleted by retention" state.
- `data-testid` on every new element.

## Out of scope

- Compaction of small files into daily or monthly ones.
- Archiving open sessions in pieces.
- Full-text search across the archive from the UI. Use DuckDB or Athena.
- Restoring an archive into PostgreSQL.
- Parquet column encryption.
- `.pcapng` captures. They keep their own lifecycle for now (see the open
  questions).

## Phasing

Each phase ships on its own and becomes one spec in `specs/todos/`.

1. **Per-level retention, delete only.** Columns, resolver, store parameters,
   sweep per resolved window, ledger rule, verifier excuse per server, API and
   UI. No bucket involved.
2. **Archiver.** `DBB_ARCHIVE_URL`, `internal/archive` (schema, writer, claim,
   audit entries), the archive and archive-delete phases of the sweep.
3. **Reading.** API fallback, raw download, UI states.
4. **Verification and docs.** `audit verify --archive`, `docs/archive.md`,
   `docs/audit-chain.md`, a website page with DuckDB and Athena recipes.

With nothing configured, behaviour is exactly today's.

## Open questions

1. **Several server groups on one server.** Recommended: keep the longest
   (largest delete window, smallest archive window). The alternative is the
   shortest delete window, which suits a privacy rule ("PII servers: 30 days")
   better than an audit rule ("prod: 1 year"). Only one can be the default.
2. **Captures.** Should the `.pcapng` object of a connection be deleted at the
   same `delete_after_days`? Recommended: yes, as a follow-up. It is the same
   promise ("this session's data is gone after N days") and it closes the
   orphaned-object caveat of `DBB_CONNECTION_RETENTION`.
3. **A preview before lowering a delete window.** A count of the connections
   that become due would make a typo (30 for 300) visible before the sweep
   acts on it. Recommended for phase 1 if cheap, otherwise a warning text.
