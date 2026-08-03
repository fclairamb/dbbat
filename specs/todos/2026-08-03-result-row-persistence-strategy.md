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

### The writer: a bounded channel drained opportunistically

Rather than a fixed flush threshold, use a buffered channel and have the writer
drain whatever happens to be queued — batches size themselves to load, with no
timer to tune:

```go
row := <-ch                       // block for the first row
batch := append(batch[:0], row)
for len(batch) < maxBatch && batchBytes < maxBatchBytes {
    select {
    case r := <-ch:
        batch = append(batch, r)
        batchBytes += r.RowSizeBytes
    default:                      // nothing queued right now — flush what we have
        break
    }
}
store.StoreQueryRows(ctx, batch)
```

Idle producer → batches of 1 and minimal latency; fast producer → full batches.
The channel cap bounds in-flight memory directly, which is what replaces
PostgreSQL's unbounded `capturedRows` hold.

Sizing: cap the drain at **~1000 rows or a byte budget (~8 MB), whichever trips
first** — not a few dozen. Batching only pays by amortizing the round-trip, and
a bulk insert of 1000 small JSONB rows costs barely more than one of 50; at
~1ms/insert a 50-row cap ceilings throughput near 50k rows/s, which is over a
minute of pure insert time on a multi-million-row capture. The byte budget
matters as much as the row count: 1000 rows is nothing for a single-column
select and tens of MB for wide rows.

**Send non-blocking, drop on full — do not let the store stall the proxy.**
A blocking send turns a slow moment in dbbat's own storage into a stall on the
customer's query, since the capture path runs inline with forwarding rows to the
client. PostgreSQL has no such coupling today (its persist is fully async after
the query completes), and this work must not introduce one:

```go
select {
case ch <- row:
default:
    pending.capturedDropped = true   // degrade capture, never the data path
}
```

Record "dropped" **distinctly from "truncated"** — truncated means a configured
limit was reached, dropped means the writer fell behind. Both make the capture
partial but for opposite reasons, and conflating them makes the UI lie. Build on
the existing `results_truncated` column (added in
[`2026-08-03-pg-result-capture-keep-rows-on-limit.md`](specs/done/2026/08/2026-08-03-pg-result-capture-keep-rows-on-limit.md))
rather than overloading it.

### Wiring it in
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
  failed flush must not kill the session (today's calls only log). A failed
  batch now loses 1..N rows mid-capture, so mark the query partial rather than
  only logging.

### Three things that will bite

1. **Row numbers must become a producer-side running counter.** PostgreSQL
   assigns them after the fact
   ([`intercept.go:422`](internal/proxy/postgresql/intercept.go:422),
   `capturedRows[i].RowNumber = i + 1`); once batches flush early those rows are
   already gone from the slice.
2. **A flush barrier is needed at query end**, not just a channel close — the
   tail must land before the query reads as complete, or the UI shows a finished
   query with rows still arriving.
3. **Cross-query batching needs a signature change.**
   `StoreQueryRows(ctx, queryUID, rows)`
   ([`queries.go:77`](internal/store/queries.go:77)) takes a single query UID, so
   a drained batch spanning concurrent sessions cannot go out in one call. Have
   it accept rows carrying their own `query_id` instead; that unlocks **one
   process-wide writer** whose batches span sessions, which is where this design
   pays off most — a busy proxy with many small result sets currently issues one
   INSERT per query and would issue one per ~1000 rows overall. Note the FK
   ordering constraint: the parent `queries` row must exist before any of its
   rows flush.

## Files

- `internal/proxy/shared/` — the new buffered writer.
- `internal/proxy/oracle/intercept.go:341` — the per-row insert.
- `internal/proxy/postgresql/session.go:527`, `intercept.go:405-430` — buffering
  and the row-numbering / `CreateQuery` ordering constraint.
- `internal/proxy/mysql/intercept.go:350`, `internal/proxy/mongodb/result.go:302`.
- `internal/store/queries.go:77` — `StoreQueryRows`, unchanged.
