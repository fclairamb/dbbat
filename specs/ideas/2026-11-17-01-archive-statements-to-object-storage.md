# Archive statements to object storage, then delete them, per server

## Goal

Move a closed connection's statements and captured result rows out of
PostgreSQL into object storage (S3) as raw columnar files, and delete them
later. Two parameters drive it:

- `archive_after`: how long after a session closes its data leaves PostgreSQL
  for the bucket.
- `delete_after`: how long before it is deleted for good.

Both are durations with one-second resolution, and both resolve at three
levels: the instance, a server group, a server.

## Why

Today retention is one instance-wide window (`DBB_QUERY_STORAGE_RETENTION`),
and the only choice is "keep it in PostgreSQL" or "delete it":

- Keeping a year of captured rows in PostgreSQL to satisfy an audit
  requirement grows the storage database with data nobody reads after the
  first week.
- A production server under a compliance rule and a staging server full of
  throwaway sessions get the same window.

An archive tier fixes the first, per-server windows fix the second.

## Measurements

Taken on 2026-11-17 on a production instance, from its whole connection
ledger. Absolute figures stay in the private notes.

- Sessions are one-shot scripts and agents, not people holding a console
  open: median 2 statements and 0.1 s, 45% have exactly one statement, p99 is
  42 statements.
- A few hundred sessions a day. Statement volume is tiny, tens of megabytes a
  month. Storage pressure can only come from `query_rows`.
- 1% of the sessions carry 89% of the bytes.
- The table sizes are **not measured yet**: that needs SQL on the store, not
  the API. They decide question 1 below.

Objects produced by each possible unit, relative to one per connection:

| Unit | Objects | Statements per object |
|---|---|---|
| One per connection | 1 | 4 |
| One per server and sweep (hourly) | 7.5 times fewer | 32 |
| One per server and day | 13 times fewer | 55 |

## Decisions

### 1. Format: Parquet

| | Parquet | ORC | Vortex |
|---|---|---|---|
| Pure-Go writer (the build is `CGO_ENABLED=0`) | Yes, two maintained: `parquet-go/parquet-go` v0.32, `apache/arrow-go` v18.9 | No official Go library. `scritchley/orc` has no release and its README says not to use the writer yet | None. One third-party read-only reader, 0 importers. Writing means cgo over the Rust library |
| Readers | DuckDB, Athena, Trino, Spark, ClickHouse, BigQuery, Snowflake, pandas, polars | Hive, Trino, Spark | Rust, Python, Java, DuckDB core extension (Linux and macOS only) |
| Stability | Frozen for years | Frozen | File format stable since 0.36, crates still 0.x (0.86+), LF AI & Data incubation |

Parquet is the only candidate dbbat can write without cgo, and the one every
query engine reads. With files this small it is chosen for its schema and its
readers, not for scan speed. If Vortex ever matters, converting is one DuckDB
`COPY`.

Library: `github.com/parquet-go/parquet-go`. Struct-tag API, pure Go, no Arrow
dependency. `arrow-go` is the fallback.

dbbat does not embed a query engine. DuckDB's Go client needs cgo.

### 2. Unit: one closed connection

One connection = one `queries` object and, when it captured rows, one `rows`
object. Only closed connections are archived.

- The `queries` audit chain is one chain per connection and `query_rows` one
  per query, so a file holds whole chains.
- Deletion is exact: each object goes at its own deadline, to the second. No
  shared file to rewrite, no batch to reason about.
- One connection row is the claim, and the key is a pure function of the row.
  It is the pattern `dump_key` already uses.

A file per server and day was considered and dropped. It would cut objects by
13, but only when windows are whole days: with a 90-second window nothing can
wait for the day to end. And the number it improves is small to begin with
(a few hundred objects a day). The files are tiny either way.

The known cost: a scan across a year opens more than 100,000 small objects.
Filtering on `date` and `server` prunes from the path. If that gets slow, a
compaction job can roll objects up later. It is not in this design.

### 3. Layout

```
<prefix>/v1/queries/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
<prefix>/v1/rows/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
```

- Table first, then hive partitions, so an engine can glob one table and prune
  on `date` and `server` from the path alone.
- `date` is the UTC date of `disconnected_at`. A retry overwrites the same
  object.
- `server` is the uid. Names are editable; the name at archival time is a
  column inside the file.
- `v1` is the schema version.
- Statements and result rows live under two prefixes on purpose, so IAM can
  give an analyst the statements without the data they returned.

### 4. Schema

`queries`, one row per statement, sorted by `chain_seq`:

| Column | Type | Note |
|---|---|---|
| `connection_uid`, `user_uid`, `server_uid`, `grant_uid` | UUID | Connection identity, repeated on every row |
| `username`, `server_name`, `protocol`, `source_ip` | string | As of archival time |
| `connected_at`, `disconnected_at` | timestamp(µs, UTC) | |
| `query_uid` | UUID | |
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
for the 1% of sessions that carry most of the bytes.

Everything a MAC covers is kept bit-exact (canonical JSON, microsecond UTC
timestamps, exact decimal), so the chains can be re-verified from the files.

### 5. Policy and resolution

Storage, following what the statement timeout already does:

- `servers.archive_after_seconds`, `servers.delete_after_seconds`: nullable
  integers.
- `server_groups`: the same two columns.
- Instance: store parameters `retention.archive_after` and
  `retention.delete_after`, Go duration strings (`90s`, `36h`, `8760h`), which
  win over the environment like `limits.statement_timeout`. Environment
  defaults: new `DBB_ARCHIVE_AFTER` (default: never), and the existing
  `DBB_QUERY_STORAGE_RETENTION` for deletion.

Three states per value: `NULL` (or unset) inherits, `0` means never and
overrides what is below, a positive value is the duration. The smallest
positive value is one second.

The API takes seconds. The UI takes a number and a unit (seconds, minutes,
hours, days) and shows the largest unit that divides the value.

Each parameter resolves on its own, in one place
(`store.ResolveRetentionByServers`, shaped like
`ResolveServerApproverGroupsByServers`: two batched queries):

1. The server's value, when not `NULL`.
2. Otherwise the server groups that set one. When several do, **keep the
   longest**: deletion takes the largest window (never beats everything),
   archival takes the smallest. Conflicting policies resolve to the least
   destructive outcome.
3. Otherwise the instance value.

After resolution, archival applies only when `0 < archive < delete` or delete
is never. Otherwise the data is simply deleted at its delete window.

Policy is read live at sweep time, never snapshotted. Editing a window, or
moving a server between groups, applies to data already stored. Lowering a
delete window destroys data at the next sweep: the UI warns at the point of
edit and the change is audited.

The resolver reads soft-deleted servers too, so their sessions keep the window
the server had. `ssh` and `kubernetes` rows have no sessions and no retention
fields.

### 6. Clocks and sweep cadence

- **Archival**: `disconnected_at + archive_after`. Open sessions are never
  archived.
- **Deletion of statements still in PostgreSQL**: `executed_at +
  delete_after`, as today, but per resolved window (one batched delete per
  distinct window, joined through `connections.database_id`).
- **Deletion of an archive**: `disconnected_at + delete_after`.
- **Ledger**: a connection row is never deleted before its statements. The
  connection sweep uses `max(connection window, resolved delete window)` and
  skips any row with a live archive.

The sweep runs every hour today. A window of seconds needs a faster one, so
the interval follows the policy:

```
interval = clamp(shortest positive window anywhere / 4, 5s, 1h)
```

recomputed each turn from the instance values and one `min()` over the four
columns. Windows of days keep the hourly sweep. A 60-second window is swept
every 15 seconds. The parameter is exact to the second; enforcement lags by at
most one interval.

### 7. Bucket

- `DBB_ARCHIVE_URL`, a `gocloud.dev/blob` URL (`s3://bucket/prefix`,
  `file://`), opened with the same helper `internal/dump/upload.go` uses. It
  may be the dump bucket with another prefix.
- Unset: archival is inert everywhere, statements stay in PostgreSQL until
  their delete window, `GET /instance` reports `archive_enabled: false` and the
  UI disables the archive fields.
- Needs `PutObject`, `GetObject`, `DeleteObject`. No `ListBucket`.
- dbbat deletes archive objects itself, because a bucket lifecycle rule cannot
  express per-server windows.
- Encryption is the bucket's (SSE-S3 or SSE-KMS).

### 8. Archiver

New package `internal/archive`. It runs as phases of the retention sweep
(`retention.go`), which gains jitter:

1. Archive due connections.
2. Delete due archives.
3. Delete due statements still in PostgreSQL.
4. Delete due ledger rows.

Per connection:

1. **Claim** with a conditional `UPDATE` on `connections.archive_claimed_at`
   (a lease, `FOR UPDATE SKIP LOCKED` on the candidate select), the
   `ClaimGrantBytesSteps` pattern. Candidates: closed, due, not archived, with
   at least one statement.
2. **Write**: stream statements in `chain_seq` order from PostgreSQL through
   the Parquet writer into the blob writer, hashing with SHA-256 on the way.
   Same for rows. No local spool.
3. **Seal**: append a chained `connection.archived` audit entry with the keys,
   sizes, SHA-256 of each object, statement and row counts, and the policy
   that applied (duration and which level it came from).
4. **Commit**, one transaction: set `archived_at`, `archive_key`,
   `archive_queries_sha256`, `archive_rows_sha256` on the connection row and
   `DELETE FROM queries WHERE connection_id = ?` (rows cascade).

A crash anywhere leaves the statements in PostgreSQL. The lease expires, the
next sweep redoes the connection, the objects are overwritten at the same key
and a second audit entry is appended. The latest `connection.archived` entry
is authoritative.

**A failing bucket never turns into deletion.** While `DBB_ARCHIVE_URL` is set
and a connection is due for archival but not archived, phase 3 skips its
statements, even past their delete window. Archive failures are logged, and
after an hour of consecutive failures a Slack message goes to the default
notify channel, coalesced like the termination notices.

Archive deletion: delete both objects, append `connection.archive_deleted`
(chained), set `archive_deleted_at` and clear the key.

Both event types are excluded from an unfiltered `GET /api/v1/audit`, like
`connection.opened` and `connection.closed`.

### 9. Audit chain

Kept minimal on purpose. The verifier already has many states, and a mistake
here produces false breaks or false excuses.

- **Archived** (`archived_at` set, statements gone from PostgreSQL): not a
  break. The row's SHA-256 values must equal the latest `connection.archived`
  entry. Counted as `chains_archived`. No bucket access.
- **Archive deleted**: excused only by a `connection.archive_deleted` entry.
  Counted as `chains_archive_deleted`.
- **Deleted from PostgreSQL without archive**: `StatementsPastRetention` takes
  the window resolved for the connection's server, with the same resolver the
  sweep uses. "Excused" keeps meaning "the sweep would have deleted it".

A tampered or missing object is detectable without trusting the bucket: its
hash is sealed in the audit chain. Walking the chains inside the files is
possible, since the MACs are there, and is deferred.

### 10. Reading

In the first version dbbat does not serve archived statements itself:

- `GET /connections/{uid}` gains `archived_at`, `archive_deleted_at` and
  `statements_location`: `store`, `archive` or `deleted`.
- `GET /connections/{uid}/archive/queries` and `/archive/rows` stream the raw
  Parquet objects, like the dump download.
- The connection page shows an Archived badge and the two downloads in place
  of the statement feed. The statement list page says it covers PostgreSQL
  only.

Everything else goes through an engine pointed at the bucket:

```sql
SELECT username, server_name, count(*) AS statements, sum(duration_ms) AS total_ms
FROM read_parquet('s3://bucket/dbbat/v1/queries/**/*.parquet', hive_partitioning = true)
WHERE date >= '2026-10-01' AND sql_text ILIKE '%customers%'
GROUP BY ALL;
```

### 11. API and UI

- `PUT /instance/retention` with the two durations (empty falls back to the
  environment). `GET /instance` returns the raw values, the resolved ones with
  their source (`parameter` or `env`), and `archive_enabled`.
- `PUT /servers/{uid}` and `PATCH /server-groups/{uid}` take
  `archive_after_seconds` and `delete_after_seconds`, tri-state in JSON: absent
  leaves it alone, `null` inherits, a number sets it.
- `GET /servers/{uid}` returns `effective_retention`: both resolved values and
  where each came from (`server`, `server_group` with the uids, `instance`).
- 400 on a negative value, or on `archive >= delete` set at the same level.
- Settings page: a Retention card. Server and server-group dialogs: two
  duration fields whose placeholder shows the inherited value and its source.
- `data-testid` on every new element.

## Out of scope

- Serving archived statements in the UI (pagination over Parquet).
- Walking the chains inside the archive files (`audit verify --archive`).
- Compaction of small files.
- Archiving open sessions in pieces.
- Restoring an archive into PostgreSQL.
- Parquet column encryption.
- `.pcapng` captures. They keep their own lifecycle for now.

## Phasing

Each phase ships on its own and becomes one spec in `specs/todos/`.

0. **Spike, one hour.** Write both schemas with `parquet-go`, read them with
   DuckDB, check that UUID, decimal, timestamp and JSON columns come out
   right and that the MAC inputs round-trip bit-exact. Note the size of a
   two-statement file.
1. **Per-level retention, delete only.** Columns, resolver, store parameters,
   adaptive sweep interval, sweep per resolved window, ledger rule, verifier
   excuse per server, API and UI. No bucket involved.
2. **Archiver.** `DBB_ARCHIVE_URL`, `internal/archive` (schema, writer, claim,
   audit entries, failure alert), the archive and archive-delete phases, the
   verifier's two archive states, the badge and the downloads,
   `docs/archive.md` with DuckDB and Athena recipes.

With nothing configured, behaviour is exactly today's.

## Open questions

1. **Archive result rows, or only statements?** Rows are the sensitive part
   and, by every indication, the bulk. The audit value is mostly in the
   statements. An instance switch (`DBB_ARCHIVE_ROWS`, default on) would let a
   deployment keep statements for a year and drop rows at archival. Needs the
   table sizes to decide.
2. **Several server groups on one server.** Recommended: keep the longest. The
   alternative, the shortest delete window, suits a privacy rule ("PII
   servers: 30 days") better than an audit rule ("prod: 1 year").
3. **Captures.** Should the `.pcapng` object of a connection be deleted at the
   same `delete_after`? Recommended: yes, as a follow-up.
4. **A preview before lowering a delete window.** A count of the connections
   that become due would make a typo visible before the sweep acts on it.
