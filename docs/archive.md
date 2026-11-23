# Archive of captured result rows

Captured result rows are 96% of the storage database. dbbat can move them to an
object-storage bucket as Parquet files once a connection has been closed for
`archive_after`, and deletes them for good after `delete_after`. Statements
(4% of the store) stay in PostgreSQL until their own delete window.

Archiving and deleting are optional and **off by default**. The archive has one
use: showing, later, what a session exchanged with the database. dbbat reads it
one session at a time; nothing searches it.

## Settings

| What | Environment | Config key | Default |
|---|---|---|---|
| Bucket | `DBB_ARCHIVE_URL` | `archive_url` | unset: archival does not exist |
| Archive after | `DBB_RETENTION_QUERIES_ARCHIVE_AFTER` | `retention.queries.archive_after` | never |
| Delete after | `DBB_RETENTION_QUERIES_DELETE_AFTER` | `retention.queries.delete_after` | never |

Archival needs both the bucket and an archive window. The windows resolve at
three levels, per parameter: the server (`archive_after_seconds`,
`delete_after_seconds` on `PUT /servers/{uid}`), else its server groups (the
longest delete window and the shortest archive window win), else the instance
(`PUT /instance/retention`, over the environment). `null` inherits, `0` means
never. Archival applies only when `0 < archive < delete` or delete is never.

## Bucket

`s3://bucket/prefix` (any S3-compatible store) or `file:///var/lib/dbbat/archive`
(a directory). See the session dumps page for Google Cloud Storage and Azure.
dbbat needs `PutObject`, `GetObject` and `DeleteObject`, never `ListBucket`.
Encryption is the bucket's (SSE-S3 or SSE-KMS). dbbat deletes archive objects
itself, because a lifecycle rule cannot express per-server windows.

With several replicas, a `file://` directory must be a shared volume.

On `file://` dbbat opens the directory with `metadata=skip` (no `.attrs` sidecar
files) and `no_tmp_dir` (atomic writes on the same file system), and removes the
`server=` and `date=` directories it empties, so the directory holds nothing but
the `.parquet` files.

## Layout

```
v1/rows/date=2026-11-17/server=<server-uid>/<connection-uid>.parquet
```

One object per closed connection that captured rows. `date` is the UTC date of
`disconnected_at`; `server` is the uid. A key only uses `a-z`, `0-9`, `-`, `=`,
`.`, `_` and `/`, built from a date and uids only, so the tree is a valid path
on any file system and can be copied out with `aws s3 sync` and read in place.
DuckDB reads it with `hive_partitioning = true`.

## Schema (`rows`)

| Column | Type |
|---|---|
| `connection_uid`, `query_uid`, `row_uid` | UUID |
| `row_number` | int32 |
| `row_data` | JSON (the text PostgreSQL's jsonb holds) |
| `row_size_bytes` | int64 |

Sorted by statement order (`executed_at`, `uid`) then `row_number`, zstd, row
groups capped at 50,000 rows. File metadata: `dbbat.schema_version`,
`dbbat.version`, `dbbat.connection` and `dbbat.query_offsets` (statement uid to
`[first row position, row count]`).

There is no per-row MAC: a row chain is whole in a file, so a verifier holding
the key recomputes it from `row_uid`, `row_number`, `row_data` and
`row_size_bytes` and compares the head with `queries.row_chain_mac`, which stays
in PostgreSQL. The file's integrity is its SHA-256, sealed in a chained
`connection.archived` audit entry (the latest one is authoritative).

## The pass

The sweeper (one per process) runs: archive due connections, delete due
archives, delete due statements, delete due ledger rows. It wakes at startup,
after a connection close, after a policy edit, and hourly; with no window set
anywhere it stays idle. A pass is bounded (200 connections, 512 MiB) and wakes
itself again when it stopped on its bound.

Per connection: claim (a lease on `connections.archive_claimed_at`), stream the
rows through the Parquet writer into the bucket hashing as it goes, seal in the
audit chain, then one transaction marks the connection archived and deletes its
rows from PostgreSQL. A crash leaves the rows in PostgreSQL; the next pass
overwrites the same key.

**A failing bucket never turns into deletion**: while `DBB_ARCHIVE_URL` is set,
statements and ledger rows of a connection due for archival but not archived are
kept even past their delete window.

PostgreSQL does not give freed space back to the disk. After the backlog is
archived, `query_rows` keeps its size and reuses the space; shrinking it is
`VACUUM FULL` or `pg_repack`, the operator's call.

## Reading

`GET /queries/{uid}/rows` serves an archived statement's rows from the bucket
with the same parameters, JSON and cursor as before. `GET /connections/{uid}`
reports `rows_location` (`store`, `archive` or `deleted`). A read that fails
(object missing, hash mismatch, bucket down, archive not configured) is a 502
saying which. Objects up to 8 MiB are fetched whole and checked against the
sealed SHA-256; larger ones are read through range requests (not hash checked).
`GET /connections/{uid}/archive/rows` downloads the raw object.

The key, size and SHA-256 a read uses come from the chained
`connection.archived` entry, which must agree with the connection row. A row
whose archive columns have no authentic entry, or disagree with it, is never
read (502, `archive reference tampered with`), and an object without a sealed
hash is a hash mismatch, never an unchecked read.

The audit verifier counts `chains_archived` and `chains_archive_deleted` and does
not treat their missing rows as a break, but only for connections whose archive
(and deletion) is sealed: the excuse is only as strong as its seal. `archived_at`
set on a row with no matching `connection.archived` entry excuses nothing, and an
entry that disagrees with the row is a break. See
[the audit chain](audit-chain.md#archived-captures).
