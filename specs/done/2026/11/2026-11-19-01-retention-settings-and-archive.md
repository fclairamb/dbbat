# Retention: uniform settings, windows per server, archive to object storage

## Goal

Four things, in this order. Each one is a phase that ships on its own.

1. **Bucket URLs.** dbbat talks to object storage through the S3 API or a
   local directory, nothing else. The docs say so.
2. **Uniform retention settings.** One config section, one naming rule and one
   set of rules for every retention window.
3. **Windows per level.** A delete window resolved at three levels: the
   instance, a server group, a server.
4. **Archive.** A closed connection's statements and captured result rows
   leave PostgreSQL for a bucket as Parquet files after `archive_after`, and
   are deleted for good after `delete_after`. dbbat reads them back.

Both windows are durations with one-second resolution.

Archiving and deleting are optional and off by default. An instance with
nothing configured archives nothing and deletes nothing.

The archive has one use: showing, later, what a session exchanged with the
database. dbbat reads it one session at a time. Nothing searches it.

## Why

- Keeping a year of captured rows in PostgreSQL to satisfy an audit
  requirement grows the storage database with data nobody reads after the
  first week.
- A production server under a compliance rule and a staging server full of
  throwaway sessions get the same window.
- The three retention settings that exist were each added on their own: three
  name shapes, three meanings of "unset", and one of them deletes on a typo.
- The docs promise `gs://` and `azblob://` buckets. The binary has neither
  driver and refuses to start on them.

## Measurements

Taken on 2026-11-17 on a production instance, from its whole connection
ledger. Absolute figures stay in the private notes.

- Sessions are one-shot scripts and agents, not people holding a console
  open: median 2 statements and 0.1 s, 45% have exactly one statement, p99 is
  42 statements.
- A few hundred sessions a day.
- 1% of the sessions carry 89% of the bytes.

Table sizes, read from the store itself:

- `query_rows` is 96% of the database. `queries` is 4%. `connections` and
  `audit_log` together are under 1%.
- The store grows by several gigabytes a month, nearly all of it captured
  rows. Nothing has ever been deleted from it.
- 30% of the statements capture rows. Among those: median 2 rows, p90 about
  100, p99 several thousand. 1% of them hold 83% of the rows, 10% hold 99%.
- A captured row weighs about 320 bytes in PostgreSQL for about 115 bytes of
  data (median 31). The rest is per-row overhead: two 32-byte MACs, a uid, the
  tuple header, the indexes.

So archival is about result rows. Archiving statements alone would move 4% of
the data.

---

## Phase 1: bucket URLs are S3 or a directory

### Today

`CLAUDE.md` (the `DBB_DUMP_UPLOAD_URL` row) and
`website/docs/features/session-dumps.md` ("`gs://` and `azblob://` work too")
say Google Cloud Storage and Azure Blob are supported.
`internal/dump/upload.go` blank-imports only `gocloud.dev/blob/fileblob` and
`gocloud.dev/blob/s3blob`, so `blob.OpenBucket` fails on the other two schemes
and dbbat refuses to start.

### Decision

dbbat keeps two drivers: `s3://` and `file://`. No `gcsblob`, no `azureblob`,
no Google or Azure SDK in the binary. Other clouds are reached through their
S3 compatibility, like MinIO, Ceph, R2 or Scaleway already are.

- **Google Cloud Storage** has an S3-compatible endpoint (the XML API, with an
  HMAC key as the access key and secret). The URL has this shape:

  ```
  s3://bucket/prefix?endpoint=https://storage.googleapis.com&region=auto&request_checksum_calculation=when_required&response_checksum_validation=when_required
  ```

  The two checksum options are there because the AWS SDK (service/s3 v1.73.0
  and later) sends checksums by default that some S3-compatible stores
  reject. Both are `gocloud.dev` URL options, already supported. **The exact
  URL is not verified.** Reports differ on the region to sign with (`auto` or
  `us-east-1`). Before the docs print a URL, run an upload, a download and a
  delete against a real GCS bucket with it.
- **Azure Blob Storage** has no S3 API. It works only behind a gateway that
  speaks S3 in front of it (S3Proxy is the open-source one). dbbat then sees
  an ordinary S3 endpoint. The docs say this in one sentence and do not
  promise more. dbbat does not test it.

### Work

- Fix the three places: `CLAUDE.md`, `website/docs/features/session-dumps.md`
  and the comment at `internal/config/config.go` (the `UploadURL` field).
  `s3://` for S3 and any S3-compatible store, `file://` for a directory.
- Add a short "Other clouds" paragraph to `session-dumps.md`: the verified
  Google URL, the Azure sentence, and `use_path_style=true` for stores that
  need it.
- `gs://` or `azblob://` in a bucket URL: fail at startup with a message that
  says the scheme is not supported and names the S3 route. Today the error is
  gocloud's "no driver registered".
- A test opens one URL per documented scheme, and checks the two refused ones
  get the new message. The list cannot drift again.

This applies to every bucket URL: `DBB_DUMP_UPLOAD_URL` today,
`DBB_ARCHIVE_URL` in phase 5. They are opened by the same helper.

---

## Phase 2: uniform retention settings

### Today

| What | Environment | Config key | Unset means | A typo (`30d`) does |
|---|---|---|---|---|
| Statements and their captured rows | `DBB_QUERY_STORAGE_RETENTION` | `query_storage.retention` | never | WARN, nothing deleted |
| Closed connections | `DBB_CONNECTION_RETENTION` | `connection.retention` | inherit the statement window | WARN, nothing deleted |
| Local capture files | `DBB_DUMP_RETENTION` | `dump.retention` | `24h` | deletes after `24h`, no log line |

- Three name shapes: `<TABLE>_STORAGE_RETENTION`, `<TABLE>_RETENTION`,
  `DUMP_RETENTION`. The second needs its own entry in `envExactKeys`, because
  no prefix rule matches it.
- Three sections of the config file.
- Three meanings of "unset".
- Two behaviours on a mistake. The dump one deletes: `runDumpCleanup` falls
  back to 24 hours when the value does not parse, and says nothing.
- The dump window is parsed in five places. Each proxy starts its own hourly
  sweep of the same directory.

### Names

`DBB_RETENTION_<OBJECT>_<ACTION>_AFTER`, and the same words as a dotted key.

| What | Environment | Config key | Default |
|---|---|---|---|
| Statements and their captured rows | `DBB_RETENTION_QUERIES_DELETE_AFTER` | `retention.queries.delete_after` | never |
| Closed connections | `DBB_RETENTION_CONNECTIONS_DELETE_AFTER` | `retention.connections.delete_after` | never |
| Local capture files | `DBB_RETENTION_DUMPS_DELETE_AFTER` | `retention.dumps.delete_after` | `24h` |
| Archive (phase 5) | `DBB_RETENTION_QUERIES_ARCHIVE_AFTER` | `retention.queries.archive_after` | never |

```yaml
retention:
  queries:
    delete_after: 720h
  connections:
    delete_after: 8760h
  dumps:
    delete_after: 24h
```

- The object is a plural noun the operator already knows: `queries` (the
  table, the API path, the `query_storage` section), `connections`, `dumps`
  (`DBB_DUMP_DIR`). The docs say "statements", the names say `queries`.
- The same dotted key is the name of the store parameter in phase 3. One name
  in the environment, the file, the store and the API.

Settings that say what is kept or where, not for how long, do not move:
`DBB_QUERY_STORAGE_STORE_RESULTS`, `DBB_QUERY_STORAGE_MAX_RESULT_ROWS`,
`DBB_QUERY_STORAGE_MAX_RESULT_BYTES`, `DBB_DUMP_DIR`, `DBB_DUMP_MAX_SIZE`,
`DBB_DUMP_UPLOAD_URL`.

### Values

- A Go duration, like every other duration in dbbat (`90s`, `720h`).
- `0` means never. Unset means the default.
- The default is never. dbbat deletes nothing it was not told to delete.
- One exception: dumps default to `24h`. A capture is a debugging file with
  the full payload of a session, it is opt-in (`DBB_DUMP_DIR`), and a spool
  that never empties fills the disk.
- No inheritance. Today an unset connection window takes the statement window.
  After this change it means never: statements can expire, the ledger stays.
  The ledger is small and it is the evidence.

### One rule for a mistake

`config.RetentionWindows()` resolves the windows. When anything is wrong,
every window is zero, nothing is deleted (dumps included), and one startup
WARN names the variable and the value. It is never a startup failure: a proxy
that keeps everything is better than a proxy that does not start.

Wrong means:

- a value that is not a Go duration, or is negative;
- a `DBB_RETENTION_*` variable or a `retention.*` key that is not in the table
  above (`DBB_RETENTION_QUERY_DELETE_AFTER`). Today a mistyped name is
  silently unset;
- a removed name that is still set, in the environment or in the file:
  `DBB_QUERY_STORAGE_RETENTION` (`query_storage.retention`),
  `DBB_CONNECTION_RETENTION` (`connection.retention`), `DBB_DUMP_RETENTION`
  (`dump.retention`). The WARN gives the new name. The old value is never
  read: this is a guard, not an alias;
- a connection window shorter than the statement window (deleting a
  connection cascades to its statements);
- a connection window set while statements are kept forever.

The last two rules exist today.

### Code

- `internal/config/config.go`: a `RetentionConfig` struct under
  `Config.Retention`. `QueryStorageConfig.Retention`, `DumpConfig.Retention`
  and the whole `ConnectionConfig` (it has no other field) go away.
- `envTransform`: one rule, `retention_<object>_<setting>` to
  `retention.<object>.<setting>`, with the object taken from a known list.
  The `connection_retention` entry of `envExactKeys` goes away.
- `RetentionWindows` gains `Dumps`. Unknown keys and the three removed names
  are found in `operatorLayer`, which holds what the operator set without the
  defaults. The removed names are one small map, old key to new variable.
- `retention.go` runs the dump sweep (`dump.CleanupOldFiles`) next to the
  other two. The five `runDumpCleanup` loops, their constants and their
  goroutine names go away.
- `store.Options.QueryRetention` is fed from `RetentionWindows().Queries`, as
  today.
- Every mention of the old names: `CLAUDE.md`, `README.md`,
  `website/docs/configuration/index.md`,
  `website/docs/features/query-logging.md`,
  `website/docs/features/session-dumps.md`, `website/docs/compliance.md`,
  `internal/api/openapi.yml`, the comments in `internal/api/audit_verify.go`
  and `internal/api/observability.go`.

### Tests

- `connection_retention_test.go`, `query_storage_retention_test.go` and the
  retention part of `dump_env_test.go` become one table test in
  `internal/config/retention_test.go`.
- Each row of the names table: the variable sets the key, and the key alone
  sets the window.
- Each kind of mistake: every window is zero and the WARN names the culprit.
- Each removed name, set in the environment and set in the file: every window
  is zero and the WARN gives the new name.
- Nothing set: statements and connections are never deleted, dumps at 24h.
- A test greps the docs and the OpenAPI file for the three removed names.
  They may appear in the changelog and in the guard's map, nowhere else.

### Breaking change

Commit and PR title: `feat(config)!: uniform retention settings`.

A deployment that still sets an old name starts, deletes nothing, and logs a
WARN with the new name at every startup until it is renamed. Nothing is lost:

- statements and connections are kept, as if no window were set;
- capture files are kept too. Without the guard, an old `DBB_DUMP_RETENTION`
  longer than `24h` would be ignored and the files deleted at the `24h`
  default, sooner than configured. With it, the spool grows until the
  variable is renamed.

The changelog lists the three renames.

---

## Phase 3: windows per level, delete only

No bucket in this phase. It ships the policy, the resolver and the sweeper.

### Policy and resolution

Storage, following what the statement timeout already does:

- `servers.archive_after_seconds`, `servers.delete_after_seconds`: nullable
  integers.
- `server_groups`: the same two columns.
- Instance: store parameters `retention.queries.archive_after` and
  `retention.queries.delete_after`, Go duration strings (`90s`, `36h`,
  `8760h`), which win over the environment like `limits.statement_timeout`.
  Environment defaults: the two variables of phase 2.

The archive columns and parameter are created here and stay inert until
phase 5.

**Both are off by default.** Every value starts unset, and unset at every
level resolves to never. A new instance, or one upgraded with nothing
configured, archives nothing and deletes nothing. No migration and no default
sets a window.

- Deletion starts when a delete window is set at some level.
- Archival needs two things: `DBB_ARCHIVE_URL` and an archive window. One
  without the other does nothing.
- The two are independent. Delete without archiving, archive without ever
  deleting, both, or neither.

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

### Clocks

- **Archival** (phase 5): `disconnected_at + archive_after`. Open sessions are
  never archived.
- **Deletion of statements still in PostgreSQL**: `executed_at +
  delete_after`, as today, but per resolved window (one batched delete per
  distinct window, joined through `connections.database_id`).
- **Deletion of an archive** (phase 5): `disconnected_at + delete_after`.
- **Ledger**: a connection row is never deleted before its statements. The
  connection sweep uses `max(connection window, resolved delete window)` and
  skips any row with a live archive.
- **Dumps**: as in phase 2.

### The sweeper

The hourly ticker of `retention.go` goes away. One goroutine per process (the
sweeper) runs a pass when it is woken, and it is woken:

- at startup;
- after each connection close: the one call site in `main.go` that wraps
  `Store.CloseConnection`, and the crash reconcile when it closes orphans;
- after a retention policy edit (instance, server group, server, group
  membership);
- once an hour.

The wake-up is a non-blocking send on a channel of size 1. Any number of
closes during a pass collapse into one more pass, and two passes never run at
once in a process. Passes are at least one second apart, the resolution of the
windows, so a storm of closes costs one pass a second.

A close alone is not enough for a short window: the session that just closed
is due in `archive_after`, not now, and nothing else may close in between. So
at a close, dbbat resolves the windows of that session's server, and when the
nearest one is under an hour it arms a timer (`time.AfterFunc`) that wakes the
sweeper at that deadline. Longer windows need no timer: the hourly pass is
soon enough.

The hourly pass is also the backstop for what the events do not cover:

- a replica that closed a session and died before its timer fired (another
  replica has no event for it);
- statements of sessions still open, deleted at `executed_at + delete_after`
  as today, which no close announces.

With windows under an hour, enforcement is exact to about a second on the
replica that closed the session. With longer windows it is exact to the hour
at worst, as today, and much better on an instance where sessions keep
closing.

With no window set at any level, the sweeper is off. It reads the policy (one
small query: is any window set, at any level?) at startup, after a policy
edit, and once an hour, so a replica learns of a window set through another
replica. While the answer is no, a connection close does not wake it and no
pass reads a session. Today the sweep goroutine is not even started when no
retention is set. It has to exist now, because a window can be set from the
UI while the process runs.

### Audit chain

`StatementsPastRetention` takes the window resolved for the connection's
server, with the same resolver the sweep uses. "Excused" keeps meaning "the
sweep would have deleted it".

### API and UI

- `PUT /instance/retention` with the durations (empty falls back to the
  environment). `GET /instance` returns the raw values, the resolved ones with
  their source (`parameter` or `env`), and `archive_enabled`.
- `PUT /servers/{uid}` and `PATCH /server-groups/{uid}` take
  `archive_after_seconds` and `delete_after_seconds`, tri-state in JSON: absent
  leaves it alone, `null` inherits, a number sets it.
- `GET /servers/{uid}` returns `effective_retention`: both resolved values and
  where each came from (`server`, `server_group` with the uids, `instance`).
- 400 on a negative value, on `archive >= delete` set at the same level, and
  on an archive window while `archive_enabled` is false.
- Settings page: a Retention card. Server and server-group dialogs: duration
  fields whose placeholder shows the inherited value and its source. An empty
  field with nothing to inherit reads "Never". The archive field is there only
  when `archive_enabled` is true.
- `data-testid` on every new element.

---

## Phase 4: spike, one hour

Before any archiver code. Write both schemas of phase 5 with `parquet-go`,
read them back with `parquet-go` and with DuckDB, and check:

- UUID, decimal, timestamp and JSON columns come out right;
- the MAC inputs round-trip bit-exact (canonical JSON, microsecond UTC
  timestamps, exact decimal);
- the size of a two-statement file, and of a 100,000-row `rows` file from a
  real session.

Write the numbers into this spec.

### Spike results (rows only, `parquet-go` v0.32.1, `CGO_ENABLED=0`)

Done on 2026-11-19 with the `rows` schema of phase 5 (`internal/archive`,
`TestSpikeFormat`). The `queries` file does not exist any more (rows only), so
only `rows` was written.

- **Types.** Read back with `parquet-go` and with DuckDB 2.0: `connection_uid`,
  `query_uid` and `row_uid` come out as `UUID`, `row_data` as `JSON` (the
  `json` tag), `row_number` as `INTEGER`, `row_size_bytes` as `BIGINT`. A tree
  `v1/rows/date=2026-11-17/server=<uid>/<conn>.parquet` reads with
  `read_parquet('v1/rows/**/*.parquet', hive_partitioning=true)` and `date` and
  `server` appear as columns. No decimal or timestamp column is needed any more
  (they belonged to `queries`).
- **Bit-exactness.** `row_data` is written as the text PostgreSQL returns for
  the `jsonb` column (`row_data::text`) and read back unchanged; the archive test
  compares the JSON of every row. A finding: the row chain MAC covers the row
  `uid` (`rowChainPayload(query_id, row.UID, row_number, row_data, size,
  prev_mac)`), which the schema table above did not list. `rows` therefore has a
  `row_uid` column, without which the chain could not be recomputed from the
  file.
- **Size.** A 3-row, 2-statement file is 2,068 bytes. 100,000 synthetic rows
  (31-byte-ish JSON, random row uids) over 40 statements is 2,298,797 bytes,
  **23.0 bytes a row** (about 6.5 without `row_uid` and `query_uid`; the uids
  dominate, and are far smaller with time-ordered v7 uids). That is against about
  320 bytes a row in PostgreSQL. A real session's 100,000-row file was not
  measured: the instance data stays private.
- **Reads.** An object up to 8 MiB is fetched whole and hash checked; the
  range-read path (`SeekToRow` over `blob.NewRangeReader`) is implemented for
  larger ones.

### Adaptation to the resolved open question (rows only)

Phases 5 and 6 were implemented for result rows only:

- No `queries` file, no `connection_id` requirement on the statement
  endpoints, no gap in the statement list, no archived state of the statement
  chains. Statements stay in PostgreSQL until `delete_after`.
- One object per connection that captured rows:
  `v1/rows/date=.../server=.../<connection-uid>.parquet`. `rows_offset` is not a
  column of a `queries` table that no longer leaves PostgreSQL: the offset of
  each statement's first row travels in the file metadata
  (`dbbat.query_offsets`), and `GET /queries/{uid}/rows` reads it from the file.
- The connection row carries `archive_key`, `archive_rows_sha256`,
  `archive_rows_bytes`, `archive_rows_count`, `archived_at`,
  `archive_deleted_at`; `GET /connections/{uid}` reports `rows_location`.
- Audit chain: a connection whose rows are archived is not a break; the
  verifier skips its row chains and counts `chains_archived` (and
  `chains_archive_deleted`). The statement chain is untouched, so the state
  table asked for before coding phase 5 reduces to: statements chain as today;
  row chain walked only while `archived_at` is NULL.

---

## Phase 5: archiver

### Format: Parquet

| | Parquet | ORC | Vortex |
|---|---|---|---|
| Pure-Go writer (the build is `CGO_ENABLED=0`) | Yes, two maintained: `parquet-go/parquet-go` v0.32, `apache/arrow-go` v18.9 | No official Go library. `scritchley/orc` has no release and its README says not to use the writer yet | None. One third-party read-only reader, 0 importers. Writing means cgo over the Rust library |
| Readers | DuckDB, Athena, Trino, Spark, ClickHouse, BigQuery, Snowflake, pandas, polars | Hive, Trino, Spark | Rust, Python, Java, DuckDB core extension (Linux and macOS only) |
| Stability | Frozen for years | Frozen | File format stable since 0.36, crates still 0.x (0.86+), LF AI & Data incubation |

Parquet is the only candidate dbbat can write without cgo. With files this
small it is chosen for its schema and its tooling, not for scan speed.

Library: `github.com/parquet-go/parquet-go`. Struct-tag API, pure Go, no Arrow
dependency. `arrow-go` is the fallback.

dbbat does not embed a query engine. DuckDB's Go client needs cgo.

### Unit: one closed connection

One connection = one `queries` object and, when it captured rows, one `rows`
object. Only closed connections are archived.

- The `queries` audit chain is one chain per connection and `query_rows` one
  per query, so a file holds whole chains.
- Deletion is exact: each object goes at its own deadline, to the second. No
  shared file to rewrite, no batch to reason about.
- One connection row is the claim, and the key is a pure function of the row.
  It is the pattern `dump_key` already uses.

A file per server and day was considered and dropped. It would cut the number
of objects by 13, but only when windows are whole days: with a 90-second
window nothing can wait for the day to end. And the number it improves is
small to begin with (a few hundred objects a day).

The number of objects costs nothing at read time. Nothing scans the archive:
dbbat opens the objects of one connection, and finds them from the connection
row.

### Layout

```
<prefix>/v1/queries/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
<prefix>/v1/rows/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
```

- Table first, then one directory per day and one per server. dbbat never
  lists them. The names are for a person browsing the bucket.
- `date` is the UTC date of `disconnected_at`. A retry overwrites the same
  object.
- `server` is the uid. Names are editable; the name at archival time is a
  column inside the file.
- `v1` is the schema version.
- Statements and result rows are two objects, so listing the statements of a
  session never downloads the data they returned.

The same key is a valid path on a plain file system, so an archive can live in
a directory (`DBB_ARCHIVE_URL=file:///var/lib/dbbat/archive`) or be copied out
of a bucket with `aws s3 sync` and read in place:

- A key uses only `a-z`, `0-9`, `-`, `=`, `.`, `_` and `/`. No `:`, no space,
  no upper case (macOS and Windows file systems ignore case). `=` is legal on
  ext4, XFS, APFS and NTFS. The archiver builds keys from a date and uids
  only, never from a server name or a username, and a test pins the charset.
- With that charset the `file://` driver maps a key to a path without
  escaping anything, on Unix and on Windows.
- The longest path component is 43 characters and the tree is three levels
  deep, far below any file system limit.
- A day directory holds one directory per server that had a session that day,
  and a server directory holds that day's sessions of that server: hundreds of
  entries at most, never one directory with a year of files.

Checked on a local directory: DuckDB reads the tree with
`hive_partitioning = true`, `=` in the directory names included.

### Schema

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
| `rows_offset` | int64, optional | Position of the statement's first row in the `rows` file. Not in PostgreSQL: the archiver computes it while writing |

`rows`, one row per captured result row, sorted by statement order then
`row_number`:

| Column | Type | Note |
|---|---|---|
| `connection_uid`, `query_uid` | UUID | |
| `row_number` | int32 | |
| `row_data` | JSON string | Canonical form. Every statement has its own columns, so this stays one JSON column |
| `row_size_bytes` | int64 | |

`rows` carries no MAC at all, neither `prev_mac` nor `mac`:

- A MAC is 32 bytes that do not compress, and the median row carries 31 bytes
  of data. On 100,000 synthetic rows the file is 39 bytes a row with the
  per-row `mac` and 6.5 without.
- Nothing is lost. A row chain is always whole in a file (retention deletes a
  statement with all its rows). With the key, a verifier recomputes the chain
  from `row_data`, from the keyed genesis, and compares its head to
  `row_chain_mac`, which the `queries` file carries. The per-row MAC only said
  which row broke.
- The file itself is protected by its SHA-256, sealed in the audit chain.

`queries` keeps `prev_mac` and `mac`, because a session can lose its oldest
statements to the delete window before it closes, and its files are small
anyway.

File key-value metadata on both: `dbbat.schema_version`, `dbbat.version`,
`dbbat.connection` (the connection row as JSON, including `query_chain_mac`,
`query_chain_len`, `query_chain_stamp_version`).

zstd everywhere. Row groups capped (50k rows) so the writer's memory is bounded
for the 1% of sessions that carry most of the bytes.

Everything a MAC covers is kept bit-exact (canonical JSON, microsecond UTC
timestamps, exact decimal), so the chains can be re-verified from the files.

### Bucket

- `DBB_ARCHIVE_URL`, a `gocloud.dev/blob` URL (`s3://bucket/prefix` or
  `file://`, as in phase 1), opened with the same helper
  `internal/dump/upload.go` uses. It may be the dump bucket with another
  prefix.
- Unset, the default: archival does not exist. No archive pass runs,
  statements stay in PostgreSQL until their delete window (forever when there
  is none), `GET /instance` reports `archive_enabled: false`, and the UI does
  not show the archive fields. The API answers 400 to an archive window, and
  `DBB_RETENTION_QUERIES_ARCHIVE_AFTER` alone is ignored with a startup WARN.
- Removed after sessions were archived: nothing new is archived, and reading
  an archived session is a 502 saying the archive is not configured. The
  objects stay in the bucket.
- Needs `PutObject`, `GetObject`, `DeleteObject`. No `ListBucket`.
- dbbat deletes archive objects itself, because a bucket lifecycle rule cannot
  express per-server windows.
- Encryption is the bucket's (SSE-S3 or SSE-KMS).

On `file://`, three things differ from a bucket, and dbbat handles them so the
directory holds nothing but the `.parquet` files:

- **No sidecar files.** The driver writes a `<name>.attrs` file next to each
  object by default. dbbat opens the archive with `metadata=skip`: the hash
  that matters is already sealed in the audit chain.
- **Atomic writes on the same file system.** dbbat sets `no_tmp_dir`, so the
  object is written as `<name>.<hex>.tmp` in its final directory and renamed.
  A reader never sees a half-written `.parquet`, and the rename never crosses
  a mount.
- **No empty directories left behind.** After deleting an object, dbbat
  removes its `server=` and `date=` directories when they are empty
  (`os.Remove`, which refuses a directory that still has entries). A bucket
  has no directories, so this only runs on `file://`.

With several replicas the directory must be a shared volume. A local disk per
replica would leave each download working on one replica only.

### The archiver

New package `internal/archive`. It runs as phases of a sweeper pass
(`retention.go`):

1. Archive due connections.
2. Delete due archives.
3. Delete due statements still in PostgreSQL.
4. Delete due ledger rows.

A pass is bounded (a number of connections and a number of bytes, both
constants chosen in this phase) and wakes the sweeper again when it stopped on
its bound. The first pass after archival is switched on meets the whole
history, tens of gigabytes on the measured instance, and must not hold the
store for hours.

PostgreSQL does not give the freed space back to the disk. After the backlog
is archived, `query_rows` stays at its old size on disk and is reused from
within. Shrinking it is `VACUUM FULL` or `pg_repack`, the operator's call:
`docs/archive.md` says so.

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
   `archive_queries_sha256`, `archive_rows_sha256`, `archive_queries_bytes`,
   `archive_rows_bytes` on the connection row and
   `DELETE FROM queries WHERE connection_id = ?` (rows cascade).

A crash anywhere leaves the statements in PostgreSQL. The lease expires, the
next pass redoes the connection, the objects are overwritten at the same key
and a second audit entry is appended. The latest `connection.archived` entry
is authoritative.

**A failing bucket never turns into deletion.** While `DBB_ARCHIVE_URL` is set
and a connection is due for archival but not archived, phase 3 of the pass
skips its statements, even past their delete window. Archive failures are
logged, and after an hour of consecutive failures a Slack message goes to the
default notify channel, coalesced like the termination notices.

Archive deletion: delete both objects, append `connection.archive_deleted`
(chained), set `archive_deleted_at` and clear the key.

Both event types are excluded from an unfiltered `GET /api/v1/audit`, like
`connection.opened` and `connection.closed`.

### Audit chain

Kept minimal on purpose. The verifier already has many states, and a mistake
here produces false breaks or false excuses.

- **Archived** (`archived_at` set, statements gone from PostgreSQL): not a
  break. The row's SHA-256 values must equal the latest `connection.archived`
  entry. Counted as `chains_archived`. No bucket access.
- **Archive deleted**: excused only by a `connection.archive_deleted` entry.
  Counted as `chains_archive_deleted`.
- **Deleted from PostgreSQL without archive**: as in phase 3.

A tampered or missing object is detectable without trusting the bucket: its
hash is sealed in the audit chain.

**Before coding this part**, read `internal/store/chain_verify.go` and
`chain_sealed.go` end to end and write the exact state table here: for each
combination of stamp, survivors, `archived_at` and `archive_deleted_at`, what
the walk reports. The three bullets above were written from a summary of the
verifier, not from its code.

### UI in this phase

An Archived badge on the connection, and the two raw downloads
(`GET /connections/{uid}/archive/queries` and `/archive/rows`, like the dump
download, hash checked). The statements of an archived session are not
readable in the UI until phase 6.

`docs/archive.md`: the layout, the schema, the bucket permissions, the
`file://` notes, the `VACUUM` note.

---

## Phase 6: reading

dbbat reads its own archive. An archived session looks the same in the UI as
one still in PostgreSQL, with an Archived badge.

**Why it is easy.** Each file is one ordered stream, and dbbat goes from a
connection row straight to it:

- The object key, its size and its SHA-256 are on the connection row
  (`archive_key`, `archive_queries_bytes`, `archive_rows_bytes`, the two
  hashes). No `LIST`, no index, no catalogue.
- `queries` is sorted by `chain_seq`.
- `rows` is sorted by statement, then `row_number`, and each statement in
  `queries` carries `rows_offset`, the position of its first row in the `rows`
  file. The rows of one statement are "row `rows_offset` to `rows_offset +
  row_chain_len`".

One primitive, in `internal/archive`:

```go
// Read returns rows [offset, offset+limit) of one archive object.
func Read[T any](ctx context.Context, b *blob.Bucket, key string, size int64, sha256 []byte, offset int64, limit int) ([]T, error)
```

- An object up to 8 MiB is fetched whole in one `GET`, checked against its
  SHA-256, and read from memory. That is nearly every object.
- A larger one is read through an `io.ReaderAt` over range requests
  (`blob.NewRangeReader`) with `parquet.OpenFile` and `SeekToRow`. Tried with
  `parquet-go` and `CGO_ENABLED=0` on a 100,000-row file: 5 reads and 11 kB to
  open it, 8 reads and 72 kB to return 50 rows from the middle. A range read
  is not checked against the hash; the download is.
- No cache. A page is one small request to the bucket.

**Paging.** The statement list pages with a `before` uid cursor today, and the
rows endpoint with its own cursor. For an archived session the handler keeps
the same parameters and the same JSON:

- statements: a session has a handful of them (p99 42), so the handler reads
  the `queries` file and applies the cursor in memory;
- rows: the cursor maps to a position from `rows_offset`, and only that range
  is read.

Check both against `Store.ListQueries` and `Store.GetQueryRows` before coding,
so the order and the cursor semantics are the same byte for byte.

**API.** The handlers switch on `archived_at`:

- `GET /connections/{uid}` gains `archived_at`, `archive_deleted_at` and
  `statements_location`: `store`, `archive` or `deleted`.
- `GET /queries?connection_id=<uid>` reads the `queries` file when the
  connection is archived.
- `GET /queries/{uid}` and `GET /queries/{uid}/rows` take the same
  `connection_id` parameter. It is required for an archived statement: once
  the row has left PostgreSQL, a statement uid alone does not say which file
  holds it. The UI always has it.
- A read that fails (object missing, hash mismatch, bucket down) is a 502 that
  says which, never an empty list.

**The archive is not searched.** It answers one question: what did this
session exchange with the database. The way in is the connection list, which
stays in PostgreSQL with its filters (user, server, server group, grant). Pick
a session, read its statements and their results.

The statement list across connections (`GET /queries` without
`connection_id`) and its text search cover PostgreSQL only, and the page says
so.

---

## Decisions taken

- **Google and Azure through S3 compatibility**, no driver of their own.
- **`queries`, not `statements`**, in the setting names.
- **Dumps keep a `24h` default.** The one exception to "never".
- **A removed setting name is a mistake, not an alias.** No old value is read,
  and it is not allowed to delete data.
- **Several server groups on one server: keep the longest.** The alternative,
  the shortest delete window, suits a privacy rule ("PII servers: 30 days")
  better than an audit rule ("prod: 1 year").
- **No per-row MAC in the `rows` file.**

## Not taken

- **A cleanup of sessions with no statement.** They are a few percent of the
  ledger, the ledger is under 1% of the store, and a session that did nothing
  is still an access. `retention.connections` covers them like any other
  session.

## Open before phase 5

- **Archive result rows only?** This spec archives statements and rows. The
  alternative keeps statements in PostgreSQL until `delete_after` (they are 4%
  of the store) and archives only the rows. It would remove the `queries`
  file, the `connection_id` requirement on the statement endpoints, the gap in
  the statement list and the archived state of the statement chains. It is a
  different design of phases 5 and 6, not a variant of this one. Phases 1 to 4
  do not depend on the answer. Recommended: rows only.

## Resolved open questions

- **Archive result rows only?** Archive result rows only (autopilot: recommended answer).
  Statements stay in PostgreSQL until `delete_after`. Phases 5 and 6 drop the
  `queries` file, the `connection_id` requirement on the statement endpoints,
  the gap in the statement list and the archived state of the statement
  chains: only the `rows` file is written and read back.

## Out of scope

- Searching the archive, in dbbat or with an engine pointed at the bucket.
- Walking the chains inside the archive files (`audit verify --archive`).
- Compaction of small files.
- Archiving open sessions in pieces.
- Restoring an archive into PostgreSQL.
- Parquet column encryption.
- Uploaded `.pcapng` captures. Their lifetime is the bucket's lifecycle rule.
  Deleting a connection's capture at the same `delete_after` is a natural
  next step.
- A preview before lowering a delete window (a count of the connections that
  become due, so a typo is visible before the sweep acts on it).
- `audit_log`, never deleted. OAuth states and stale instance rows have a
  fixed lifetime and no setting.

## Order

Phases 1 to 6, in that order. Each one leaves the build green and is worth
its own PR: phase 2 is a breaking change, phases 5 and 6 are large.

With nothing configured, behaviour is exactly today's: nothing is archived and
nothing is deleted. Each phase has a test for that.
