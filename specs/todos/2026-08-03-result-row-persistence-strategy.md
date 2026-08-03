# Result-row persistence is pathological at both ends: Oracle inserts per row, PostgreSQL buffers the whole capture

## Goal

Give all four protocols one shared, batched, bounded writer for captured result
rows: flush every N rows (or M bytes) instead of either one `INSERT` per row
(Oracle) or one `INSERT` of the entire capture held in RAM until the query ends
(PostgreSQL, MySQL).

## Why

`StoreQueryRows` ([`internal/store/queries.go:77`](internal/store/queries.go:77))
is a single bulk `INSERT` of whatever slice it is handed. What each protocol
hands it differs wildly:

| Protocol | Pattern | Call site |
|---|---|---|
| PostgreSQL | accumulate the whole capture in RAM, one `INSERT` at query end, async | [`intercept.go:426`](internal/proxy/postgresql/intercept.go:426) |
| MySQL | accumulate the whole result set, one `INSERT`, async | [`intercept.go:350`](internal/proxy/mysql/intercept.go:350) |
| MongoDB | one `INSERT` per wire reply (`firstBatch`/`nextBatch`), async | [`result.go:302`](internal/proxy/mongodb/result.go:302) |
| Oracle | **one `INSERT` per row, synchronous on the capture path** | [`intercept.go:341`](internal/proxy/oracle/intercept.go:341) |

Two distinct problems:

**1. Oracle: a DB round-trip per captured row, inline.** `captureRow` calls
`StoreQueryRows` with a one-element slice, with no goroutine — the comment says
"Stream row directly to database". Every captured row therefore blocks the proxy
until dbbat's own store acknowledges an `INSERT`. At the default
`max_result_rows = 100000` that is up to 100 000 sequential round-trips added to
a single proxied query. MongoDB's per-reply batching is the sane middle ground
and is the shape to generalise.

**2. PostgreSQL/MySQL: the whole capture sits in RAM until the query ends.**
`capturedRows` grows per `DataRow`
([`session.go:527`](internal/proxy/postgresql/session.go:527)) and is only handed
off in `persistQueryAsync` at `ReadyForQuery`. Peak retention is bounded only by
`max_result_bytes` — 100 MB by default, per concurrent in-flight query.

Note (2) got slightly worse in
[`2026-08-03-pg-result-capture-keep-rows-on-limit.md`](specs/done/2026/08/2026-08-03-pg-result-capture-keep-rows-on-limit.md):
before that change, hitting a limit nulled `capturedRows` and freed the buffer
for the remainder of a long query; now the prefix is deliberately retained until
the query completes. Peak memory is unchanged (those rows were already
accumulated when the limit tripped), but the hold now lasts the full query — for
a multi-minute scan that is up to 100 MB pinned for minutes rather than released
early. That was the right trade for correctness (the prefix is the whole point),
but it makes incremental flushing the proper fix rather than a nice-to-have.

No GitHub issue filed yet — one should be opened.

## Implementation

- Add a small `rowbuf` writer in `internal/proxy/shared/` holding a slice plus
  running byte count, with `Add(row)` flushing via `StoreQueryRows` once it
  crosses a threshold (start at 1000 rows / 8 MB, both configurable), and
  `Flush()` for the tail at query end.
- **Oracle** ([`intercept.go:341`](internal/proxy/oracle/intercept.go:341)):
  replace the per-row call with `rowbuf.Add`, and flush in `finalizeQuery`. This
  is the biggest win and the lowest risk — the row numbering already increments
  independently (`pending.rowNumber`).
- **PostgreSQL** ([`session.go:527`](internal/proxy/postgresql/session.go:527)):
  swap `capturedRows` for the buffer so memory is bounded by the flush threshold
  rather than by `max_result_bytes`. Careful: row numbers are currently assigned
  at the end (`capturedRows[i].RowNumber = i + 1`,
  [`intercept.go:422`](internal/proxy/postgresql/intercept.go:422)) — they must
  become a running counter, since flushed batches are gone by then.
  Careful too: the query row must exist before rows can reference it
  (`query_id` FK), so the `CreateQuery` currently done in `persistQueryAsync`
  has to move ahead of the first flush — Oracle already does this in
  `persistQueryRecord`, so follow that ordering.
- **MySQL** ([`intercept.go:350`](internal/proxy/mysql/intercept.go:350)): same
  swap; lower priority, a MySQL result set is materialised by the driver anyway.
- **MongoDB**: already per-reply; just route it through the same writer.
- Behaviour to preserve: `results_truncated` must still be accurate, and a
  failed flush must not kill the session (today's calls only log).

## Files

- `internal/proxy/shared/` — the new buffered writer.
- `internal/proxy/oracle/intercept.go:341` — the per-row insert.
- `internal/proxy/postgresql/session.go:527`, `intercept.go:405-430` — buffering
  and the row-numbering / `CreateQuery` ordering constraint.
- `internal/proxy/mysql/intercept.go:350`, `internal/proxy/mongodb/result.go:302`.
- `internal/store/queries.go:77` — `StoreQueryRows`, unchanged.
