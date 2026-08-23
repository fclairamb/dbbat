# `?active=true` on /connections is served by an index that was built for something else

## Goal

Give the "still open" filter an index that both restricts *and* orders, so a
page of live sessions streams instead of reading every live session and sorting
it.

## Why

`2026-08-22-01-connections-active-filter-server-side.md` moved the toggle
server-side and stated that the supporting index already existed
(`20260803010000_connections_disconnected_at_index.up.sql`). That is not quite
right, and it is worth writing down before somebody trusts it: that index is

```sql
CREATE INDEX idx_connections_disconnected_at ON connections (disconnected_at)
    WHERE disconnected_at IS NOT NULL;
```

— partial on the **closed** half, built for the retention sweep. It cannot
serve `disconnected_at IS NULL` at all.

What actually serves the filter today is `idx_connections_instance_id_open`
(`20260803020000`), partial on `disconnected_at IS NULL` and built for the
crash reconcile. Measured (`TestActiveOnlyPaginationUsesAnIndex`), PostgreSQL
scans the whole of that partial index and sorts the result:

```
Limit
  ->  Sort  (rows=4)
        Sort Key: uid DESC
        ->  Index Scan using idx_connections_instance_id_open on connections
```

That is a fine plan while live sessions are few — the sort is bounded by the
concurrency of the deployment, not by the size of the table, which is why the
test passes. It stops being fine on an instance with thousands of concurrent
sessions: every page of `?active=true` then reads and orders all of them to
return fifty rows. Nothing warns; the page just gets slower as adoption grows.

The filter also has no index that survives the reconcile index being changed.
`idx_connections_instance_id_open` exists to answer "which of *this instance's*
connections are still open"; if that query is ever reshaped (say to
`(instance_id, run_id)` or dropped for a `run_id` equivalent), the active
filter silently falls back to a backwards `connections_pkey` walk that filters
— unbounded when live sessions are rare, which is the common case.

## Implementation

- New migration adding
  `CREATE INDEX idx_connections_open_uid ON connections (uid DESC) WHERE disconnected_at IS NULL;`
  Both halves matter: the predicate restricts to live sessions, the key
  supplies the `ORDER BY uid DESC` the listing always asks for, so the page
  streams and stops after `limit` rows with no Sort node.
- Tighten `TestActiveOnlyPaginationUsesAnIndex` in
  `internal/store/observability_filters_test.go` to the stronger property
  (`assertOrderedIndexWalk`) once the index exists, and drop the
  `idx_connections_instance_id_open` allowance it currently passes to
  `assertPaginationNeverSortsTheTable` — that allowance exists only because no
  index orders the live sessions today.
- Consider whether the fixture should also cover the *many* live sessions case
  (a few hundred open at once), since that is the shape the current plan
  degrades on and the one the test cannot presently distinguish.
- Worth checking at the same time whether `idx_connections_instance_id_open`
  becomes redundant for the reconcile once this exists. It probably does not —
  the reconcile filters on `instance_id`, which the new index does not carry —
  but two partial indexes over the same rows deserve a sentence in the
  migration saying why both are kept.
