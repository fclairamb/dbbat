---
model: opus
effort: high
---

# An idle DataGrip session is killed by the statement timeout an hour after its last statement

## Problem

On 2026-09-17 dbbat 0.29.0 (`dbbat.tools.stonal.io`, build `61588c9`) ended a
session on a production read replica with

> statement exceeded the 1h0m0s limit (ran 1h0m2.1s)

and posted it to Slack. Connection `01a0af43-f8b0-78a1-aa23-c3ffd3af7069`,
client DataGrip (`application_name = 'IntelliJ IDEA 2026.2.3'`,
pgjdbc underneath). The query row completed with that error is
`01a0af43-fd56-7d79-989b-7266e6249594`, `SHOW TRANSACTION ISOLATION LEVEL`,
which the session's own capture shows upstream answering in 1ms.

The session ran eleven statements in its first 820ms and then nothing at all.
The client was idle for an hour, the backend was idle for an hour, and dbbat
killed it anyway. Nothing about that was the user's doing. This is dbbat's
bookkeeping going wrong, not an idle-connection policy: dbbat has no
idle-connection policy, on purpose, and must not grow one by accident.

### What the capture shows

Decoded from the session's `.pcapng`. Every statement went through the
extended protocol as `Parse` / `Bind` / (`Describe`) / `Execute` / `Sync`;
"entry" is a `pendingQuery` in `extendedState.pendingQueries`, appended by
`handleExecute`.

| t | client statement | upstream terminator | queue after |
|---|---|---|---|
| 19 to 145ms | four `SET` / `select version()` | `CommandComplete` each | empty |
| 173ms | `""` (the empty statement), `Execute maxRows=0` | `NoData`, **`EmptyQueryResponse`** | 1 stale |
| 206ms | `select current_database() …` | `CommandComplete` at 209ms pops the `""` entry | 1 stale |
| 234ms | `SELECT t.* FROM public.<table> t LIMIT 501`, **`Execute maxRows=501`** | `RowDescription`, 501 `DataRow`, **`PortalSuspended`** at 329ms | 2 stale |
| 374 to 438ms | three pgjdbc catalog lookups | each `CommandComplete` pops the entry two places back | 2 stale |
| 817ms | `SHOW TRANSACTION ISOLATION LEVEL` | `CommandComplete "SHOW"` at 818ms pops `SELECT n.nspname …` | `SELECT typinput …` (queued 12:07:43.323) and `SHOW …` (12:07:43.702) |
| 12:07:43.7 to 13:07:45 | nothing | nothing | clock armed on 12:07:43.323 |
| 13:07:45.4 | | | `LimitGuard` trips at limit + 2s grace, "ran 1h0m2.1s" |

The queries page confirms the shift: from the first slip on, each row's
logged duration is the time to the *next* statement's completion, not its own.
`current_database()` is logged at 204ms for a 3ms round trip, `LIMIT 501` at
192ms for 95ms, the last catalog lookup at 393ms for 4ms. The
`SELECT typinput …` row has no duration and no error (its result capture
inserted the row, nothing ever completed it), and the `SHOW` row carries the
timeout.

### Root cause

`proxyUpstreamToClient` pops `extendedState.pendingQueries` on
`CommandComplete` and `ErrorResponse` only
(`internal/proxy/postgresql/session.go:1055-1071`). The protocol says an
`Execute` ends with one of **four** backend messages:

| reply | when | dbbat today |
|---|---|---|
| `CommandComplete` | the portal ran to the end | pops |
| `ErrorResponse` | it failed | pops |
| `EmptyQueryResponse` | the statement was the empty string | **not popped** |
| `PortalSuspended` | `Execute` carried a row limit and the portal has more rows | **not popped** |

Neither missing case is exotic. The capture shows DataGrip sending the empty
statement right after it sets its `application_name`, and DataGrip pages every
result grid with `Execute maxRows=<page size>`, so opening any table with at
least 501 rows leaves a stale entry. From the first stale entry on, every pop
takes the wrong item, the last N statements of the session are never popped,
`refreshStatementClock` (session.go:613) keeps the oldest of them as "executing
upstream", and the watchdog fires `limit + 2s` after the *last* statement of an
idle session. With the 1h instance-wide limit that is every DataGrip session
left open across lunch. With a definition-level 30s limit it would be every
DataGrip session, 32 seconds after its first paged grid.

Three smaller wrongs ride on the same slip:

- **The queries page lies about durations** whenever an entry is stale, as
  above.
- **The termination names the wrong statement.** The watchdog measures the
  *oldest* pending entry (`refreshStatementClock`), but
  `persistTerminatedQuery` → `persistAbortedQuery` → `getCurrentPendingQuery`
  completes the *newest* (session.go:865), and `noteTermination` →
  `inFlightQueryUID` (session.go:564) reports the newest too. Here the clock
  measured `SELECT typinput …`; the row, the audit entry and the Slack message
  all blamed `SHOW …`.
- **A `CancelRequest` is sent for an idle backend.** `cancelUpstreamStatement`
  only asks whether the clock is running. Harmless, but it is noise in the
  target's log and it names a backend that is doing nothing.

`ReadyForQuery` is the one message that could have caught all of this and did
not. Upstream sends it to answer the client's `Sync`, and the protocol
guarantees that when it arrives nothing from before that `Sync` is still
running. dbbat clears `currentQuery` there and recomputes the clock, but the
recomputation reads the stale queue, so its answer is stale too.

## Fix

### 1. One `Execute`, one terminator

Pop on `PortalSuspended` and `EmptyQueryResponse` exactly as on
`CommandComplete` and `ErrorResponse`, in the same `bookMu` region. Both
complete the row: `EmptyQueryResponse` with no rows affected and no error,
`PortalSuspended` with whatever the capture counted (there is no command tag).

A suspended portal is **not** a running statement. The backend is idle, waiting
for the client to ask for the next page, and the server's own
`statement_timeout` agrees: per the PostgreSQL docs it is cancelled by the
completion of each `Execute` and restarted by the next query-related message.
So the clock stops at `PortalSuspended`. The follow-up `Execute` on the same
portal is already queued by `handleExecute` (`intercept.go:344`) as its own
`pendingQuery`, with the portal's SQL and parameters, and gets its own
terminator, so a grid paged N times logs N rows, each with that page's real
duration. That is the honest shape and the one the target itself counts.
Merging the pages into one row would need the clock to pause and resume per
page, which is exactly the kind of ad hoc state this incident is about. An
`Execute` on a portal dbbat no longer knows (after `Close 'P'`, or after a
`Sync` ended the transaction and the unnamed portal with it) is already a WARN
and a no-op (`intercept.go:330`), unchanged.

The simple-query path needs nothing: `Query ""` sets `currentQuery` and
`ReadyForQuery` logs it, and `PortalSuspended` cannot occur there.

### 2. Reconcile the queue at every `ReadyForQuery`

Rule 1 closes the two known holes. It does not close the class. An upstream
`ErrorResponse` mid-batch makes the server discard the rest of the batch until
`Sync`, so a second `Execute` the client had already sent gets no terminator at
all (dbbat's `errorUntilSync` only covers refusals *dbbat* made). And any
future gap of the same kind would reproduce this incident, an hour after
whatever triggered it. The protocol offers a hard backstop and dbbat should use
it.

- The client leg counts the messages that each earn exactly one
  `ReadyForQuery`: every forwarded `Sync` and every forwarded simple `Query`. A
  refused simple `Query` is answered by dbbat and never reaches upstream, so it
  is not counted; a refused extended batch still has its `Sync` forwarded
  (`discardUntilSync`), so that one is.
- Each `pendingQuery` records that count when it is queued (`syncEpoch`).
- The upstream leg counts `ReadyForQuery`. On each one, every pending entry
  whose `syncEpoch` is **below** the new count is over by definition: it is
  logged with the `ReadyForQuery`'s time as its end and an error text along the
  lines of `no completion message from upstream`, a `WARN` names the statement
  and the messages seen since its `Execute`, and it is dropped. Then the clock
  is recomputed, and now cannot keep an entry from a finished batch.

Pipelined clients (pgx, libpq 14+) send batch 2's `Parse`/`Bind`/`Execute`/
`Sync` before batch 1's `ReadyForQuery` arrives. Their entries carry the higher
epoch and survive the first `ReadyForQuery`, which is why this counts rather
than drains. Check where the counters start: the startup `ReadyForQuery` and
the ones dbbat's own pinning statements earn (`SET SESSION statement_timeout`,
the read-only pin) happen before the relay loops, so confirm
`proxyUpstreamToClient` never sees a `ReadyForQuery` without a client-side
counterpart, or seed the counter.

The backstop is a safety net, not the mechanism: a test asserts the `WARN` is
**not** emitted under the normal flows (rule 1 handles them), so a new gap
shows up as a failing assertion in the suite rather than as a killed session in
production.

### 3. The termination names the statement the clock measured

Add an `oldestPendingQuery()` next to `getCurrentPendingQuery()` and use it
from `persistTerminatedQuery`, `noteTermination` / `inFlightQueryUID` and the
Slack payload, so the row completed with `statement timeout: limit …, ran …`,
the audit entry and the notification all point at the statement whose duration
was measured. The other pending entries are completed as aborted by the
teardown, without the limit text. `persistAbortedQuery`'s byte attribution
stays on the newest entry; that is what the streamed bytes belong to.

## Tests

- **Unit**, next to `TestHandleExecute_QueuesQuery` (`intercept_test.go`):
  feed the upstream switch `EmptyQueryResponse` then `ReadyForQuery`, and
  `RowDescription` + `PortalSuspended` + `ReadyForQuery`; assert the entry is
  popped, the row is logged with its own duration, and
  `statementClock.Running()` is false afterwards. Then the DataGrip sequence
  from the capture as one table-driven case: every logged duration must be the
  statement's own, and the clock must be clear at the end.
- **Unit, backstop**: `Execute` #1 answered by `ErrorResponse`, `Execute` #2 in
  the same batch discarded by the server, `Sync`, `ReadyForQuery`; assert #2 is
  finished at the `ReadyForQuery` with the WARN, and that the WARN is absent
  from the normal cases above. A pipelined case: batch 2 queued before
  `ReadyForQuery` 1, which must not finish it.
- **Integration** (`statement_timeout_integration_test.go`,
  `//go:build integration`): a 2s limit, the DataGrip sequence driven with raw
  `pgproto3` frames (an empty statement, then `Execute maxRows=N` over a table
  with more than N rows), then sleep past `limit + grace + poll` and assert the
  session is alive (a further statement succeeds) and no `statement_timeout`
  termination was recorded. Today this test fails at the sleep.
- **Termination attribution**: a real timeout with two entries in flight; the
  row and the audit entry must name the oldest.

The prod capture is not a fixture: it holds production result rows
and `dump anonymise` strips session metadata, not data.

## Docs

- `docs/postgresql.md`, the Layer 2 paragraph (line ~193): the four
  terminators, that a suspended portal is not a running statement, and the
  `Sync`-boundary reconcile.
- `CLAUDE.md`, "Per-statement time limits": one sentence on the reconcile.

## Until it ships

Every DataGrip session that opens a grid of at least 501 rows, or sends the
empty statement, is killed `limit + 2s` after its last statement, and the Slack
message blames a statement that ran in milliseconds. With
`DBB_STATEMENT_TIMEOUT=1h` on `dbbat.tools.stonal.io` that is any DataGrip
session left idle for an hour. No configuration avoids it short of removing
the limit.

## Implementation Plan

1. **`syncEpoch` on `pendingQuery` + client-side counter** (`session.go`): add
   `syncEpoch uint64` to `pendingQuery` and `clientSyncEpoch uint64` to
   `Session` (guarded by `bookMu`, which every writer already holds). Count in
   `interceptClientMessage`'s switch: every forwarded `*pgproto3.Sync` and every
   forwarded simple `*pgproto3.Query` bumps the counter. A refused simple Query
   never reaches the switch's forward path (intercept returns an error before
   forwarding), so it is not counted, which is what the spec requires; a refused
   extended batch still forwards its Sync (discardUntilSync), so that one is.
   Stamp the epoch in `handleQuery` (simple path, on `currentQuery`) and in
   `handleExecute` (extended path, on the queued `pendingQuery`).
2. **Rule 1 — pop on all four terminators** (`proxyUpstreamToClient`): add
   `*pgproto3.EmptyQueryResponse` and `*pgproto3.PortalSuspended` cases beside
   `CommandComplete`/`ErrorResponse`, sharing one `popPendingQuery(rowsAffected,
   queryError)` helper that promotes `pendingQueries[0]` to `currentQuery` under
   `bookMu`. `PortalSuspended` pops with no rowsAffected (there is no command
   tag); `EmptyQueryResponse` pops with neither. Both then fall through to the
   same `ReadyForQuery`-less completion the other two use — note that today the
   pop only stages into `currentQuery` and `logQuery` runs at `ReadyForQuery`;
   that staging is what "completes the row" means here and it is kept, so
   durations stay wired to the `Sync` boundary.
3. **Rule 2 — reconcile at every `ReadyForQuery`**: in the `ReadyForQuery` case,
   after the existing `currentQuery` completion, walk `pendingQueries` and finish
   every entry with `syncEpoch < clientSyncEpoch`: log it with the
   `ReadyForQuery`'s time as its end and error text `no completion message from
   upstream`, emit one `WARN` naming the statement and the epochs, and drop it.
   Then `refreshStatementClock()` (already called after the switch). Seeding:
   `runUpstreamSetup` consumes the startup `ReadyForQuery` inside
   `connectUpstream`, before the relay loops start, so `proxyUpstreamToClient`
   never sees a `ReadyForQuery` without a client-side counterpart — no seed
   needed; assert this in the unit tests (the WARN must be absent from normal
   flows).
4. **Rule 3 — oldest-entry attribution**: add `oldestPendingQuery()` beside
   `getCurrentPendingQuery()`; use it in `inFlightQueryUID` and in
   `persistTerminatedQuery` (which completes the oldest with the limit text);
   `persistAbortedQuery` keeps `getCurrentPendingQuery` (newest) for byte
   attribution, but the terminated path must complete the *other* pending
   entries as aborted too, so the teardown leaves no row that reads as still
   running. Slack attribution follows automatically: `Termination.QueryUID` →
   `buildTerminationEvent` → `ev.QueryHead`.
5. **Tests**:
   - Unit (`intercept_test.go` / new `session_bookkeeping_test.go`): drive
     `proxyUpstreamToClient` over `net.Pipe` (the harness
     `TestSession_ProxyUpstreamToClient_ByteLimitAbort` already builds) with
     `EmptyQueryResponse` → `ReadyForQuery`, and `RowDescription` + 501
     `DataRow` + `PortalSuspended` → `ReadyForQuery`; assert the entry pops, the
     row logs with its own duration, `statementClock.Running()` is false. Then
     the DataGrip capture sequence as one table-driven case: every logged
     duration is the statement's own, clock clear at the end, and the reconcile
     WARN absent.
   - Unit backstop: `Execute` #1 answered by `ErrorResponse`, `Execute` #2
     discarded by the server, `Sync`, `ReadyForQuery` → #2 finished at the
     ReadyForQuery with the WARN; a pipelined case (batch 2 queued before
     `ReadyForQuery` 1) must survive.
   - Integration (`statement_timeout_integration_test.go`): 2s limit, raw
     `pgproto3` frames through a real proxy fixture (empty statement, then
     `Execute maxRows=N` over a table with more rows), sleep past
     `limit + grace + poll`, assert the session is alive and no
     `statement_timeout` termination was recorded.
   - Termination attribution: a real watchdog timeout with two entries in
     flight; the query row, the audit entry and the Slack event must name the
     oldest.
6. **Docs**: `docs/postgresql.md` Layer 2 paragraph (four terminators,
   suspended-portal-is-idle, Sync-boundary reconcile); one sentence in
   `CLAUDE.md`'s "Per-statement time limits" Layer 2 bullet.
