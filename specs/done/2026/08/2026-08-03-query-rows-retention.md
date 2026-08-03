# `query_rows` has no retention — captured results are stored forever

## Goal

Add a retention sweep for captured query results (`query_rows`, and the parent
`queries`/`connections` rows), configurable the way dump files already are, so
the observability store stops growing monotonically with proxied traffic.

## Why

The initial schema comments the table as "Store query result/COPY data rows
(with retention limits)"
([`internal/migrations/sql/20260107000000_initial_schema.up.sql:106`](internal/migrations/sql/20260107000000_initial_schema.up.sql:106)),
but no retention was ever implemented. Grepping the tree, the only statement
that ever removes a `query_rows` row is `DropAllTables`
([`internal/store/store.go:177`](internal/store/store.go:177)), which is test-mode
only. The sole periodic sweep in the codebase is the dump-file cleanup ticker
([`internal/proxy/postgresql/server.go:226`](internal/proxy/postgresql/server.go:226),
[`internal/proxy/oracle/server.go:214`](internal/proxy/oracle/server.go:214)).

So packet captures respect `DBB_DUMP_RETENTION` (default 24h) while the decoded
result rows — the same data, in a more queryable form — are kept indefinitely.
That is the wrong way round for both disk growth and data minimisation: every
row a user reads through the proxy is retained in `row_data JSONB` with no
expiry, which is awkward to defend under a data-retention policy.

Right now the only thing bounding the table's growth is the per-query capture
cap (`query_storage.max_result_rows` / `max_result_bytes`), which is a per-query
memory guard, not a retention policy — it bounds one query, not the accumulation
of every query ever proxied. See
[`2026-08-03-pg-result-capture-keep-rows-on-limit.md`](2026-08-03-pg-result-capture-keep-rows-on-limit.md);
fixing that one makes captures land more often, so this matters more, not less.

No GitHub issue filed yet — one should be opened.

## Implementation

- Config: `DBB_QUERY_STORAGE_RETENTION` in `QueryStorageConfig`
  ([`internal/config/config.go:43`](internal/config/config.go:43)), defaulting to
  something conservative (30d) with `0` meaning "keep forever" so existing
  deployments do not silently start deleting audit data on upgrade. Document it
  in the env-var table in `CLAUDE.md` and the README.
- Store: `CleanupOldQueryRows(ctx, olderThan)` in
  [`internal/store/queries.go`](internal/store/queries.go), deleting via the
  parent's timestamp — `DELETE FROM queries WHERE executed_at < $1` already
  cascades to `query_rows` (`ON DELETE CASCADE`,
  [initial_schema.up.sql:109](internal/migrations/sql/20260107000000_initial_schema.up.sql:109)).
  Delete in bounded batches (`LIMIT`, loop) so a first run against a large table
  does not take a long lock or blow up WAL.
- Index: `idx_queries_executed_at` already exists
  ([initial_schema.up.sql:102](internal/migrations/sql/20260107000000_initial_schema.up.sql:102)),
  so the sweep predicate is covered.
- Scheduling: one sweep owned by the server rather than per-protocol — the dump
  cleanup ticker is duplicated across the PG and Oracle servers already, so
  prefer a single goroutine started next to the API server, not a fifth copy.
- Decide and document whether `connections` rows outlive their queries (a
  connection with all its queries reaped is a dangling record in the UI).

## Files

- `internal/config/config.go:43` — `QueryStorageConfig`, new retention field.
- `internal/store/queries.go` — the batched delete.
- `internal/migrations/sql/20260107000000_initial_schema.up.sql:106` — the stale
  "(with retention limits)" comment this closes.
- `internal/proxy/postgresql/server.go:226` — existing ticker to model it on.
