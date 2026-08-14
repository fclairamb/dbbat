# Oracle: decide what a piggyback re-execution of an untracked cursor should do

**No GitHub issue filed yet — one should be.**

## Goal

Close (or consciously keep) the asymmetry between the two cursor re-execution
frames: a SQL-less `OALL8` naming a cursor dbbat never saw parsed **fails
closed** under a restrictive grant, while the piggyback re-execution (func
`0x03`, sub-op `0x4e` / `0x04`) naming an untracked cursor is **forwarded** under
any grant.

## Why

`specs/done/.../2026-08-08-oracle-capture-a-real-cursor-reexecution.md` measured
what clients actually send: go-ora, python-oracledb thin and JDBC thin all
re-execute with the piggyback frame, and none of them ever sends the SQL-less
`OALL8`. So the frame that fails closed is the one nobody sends, and the frame
everybody sends is the one that does not.

That was not an oversight — failing the piggyback frame closed today would be
dangerous. dbbat only knows which statement a cursor holds because it *learned*
the id from the server's response (`learnCursorID` / `findCursorIDInResponse`),
and that learning is an anchored scan over an OER, not a guarantee. Every miss
would turn into `ORA-01031` on the second execution of an ordinary read-only
session — python-oracledb reaches that path with a plain `cur.execute()` loop.

So the question is real but needs the learning to be trustworthy first.

## Implementation

- Measure how often learning actually misses. Add a counter (or a WARN already
  present in `handlePiggybackReexec`) and run the Oracle integration suite plus
  a few real client workloads: bind-heavy statements, multiple concurrent
  cursors, REF cursors, PL/SQL blocks, a statement cache under churn.
- Harden the learning if it does miss. `findCursorIDInResponse` takes the first
  OER-shaped run whose error code is success or ORA-01403; a stricter anchor
  would be to correlate the OER's sequence number with the TTC sequence byte of
  the request that opened the call, which the session does not track today.
- Then decide: either make the piggyback path fail closed like
  `refuseUnknownCursor` (same `hasStatementControls` test), or document the split
  as permanent in `docs/approvals.md` with the measurement behind it.
- While in there, the sibling gap is still open:
  `specs/todos/2026-08-08-oracle-ofetch-unknown-cursor-and-query-quota.md`.

Key files: `internal/proxy/oracle/intercept.go` (`handlePiggybackReexec`,
`learnCursorID`, `refuseUnknownCursor`), `internal/proxy/oracle/ttc_oer.go`
(`findCursorIDInResponse`), `internal/proxy/oracle/cursor_reexec_replay_test.go`,
`docs/approvals.md`.

## Resolved open questions

> Then decide: either make the piggyback path fail closed like
> `refuseUnknownCursor` (same `hasStatementControls` test), or document the split
> as permanent in `docs/approvals.md` with the measurement behind it.

**Decision — measure, harden, then fail closed.** The end state is symmetry: an
untracked cursor on the piggyback path must be refused under the same
`hasStatementControls` test as `refuseUnknownCursor`, matching the sibling
decision taken for `OFETCH` in
`2026-08-08-oracle-ofetch-unknown-cursor-and-query-quota.md`. But do **not**
flip it first — work the spec's `## Implementation` sequence in order:

1. **Measure** how often cursor-id learning actually misses. Instrument
   `handlePiggybackReexec` / `learnCursorID` with a counter and a WARN, then run
   the Oracle integration suite plus real client workloads that stress it:
   bind-heavy statements, multiple concurrent cursors, REF cursors, PL/SQL
   blocks, and a statement cache under churn.
2. **Harden** the learning if the measurement shows any miss.
   `findCursorIDInResponse` currently takes the first OER-shaped run whose error
   code is success or ORA-01403; correlating the OER's sequence number with the
   TTC sequence byte of the request that opened the call is the stricter anchor
   the spec sketches (the session does not track that today).
3. **Then fail closed**, with a replay test proving an untracked piggyback frame
   is refused under a statement-shaped control and still forwarded without one,
   and update `docs/approvals.md` so the split is recorded as closed rather than
   open.

If — and only if — the measurement shows learning misses that cannot be made
reliable by step 2, stop before step 3 and report that back rather than shipping
a change that breaks the second execute of an ordinary read-only session.

## Measurement

Run against a real `gvenzl/oracle-free:23-slim` through the real proxy, by
`TestIntegration_CursorIDLearningMissRate` in
`internal/proxy/oracle/cursor_learning_integration_test.go`. Workloads: prepared
SELECT loop, bind-heavy re-executions, three cursors interleaved on one session,
prepared DML, an anonymous PL/SQL block, a REF cursor, a statement retried after
it failed, and a statement cache churned past 40 statements. Counting is off the
proxy's own log records — no instrumentation was left in the production path.

Reproduce:

```bash
ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim \
  go test -tags integration -timeout 40m -count=1 -v \
  -run TestIntegration_CursorIDLearningMissRate ./internal/proxy/oracle/
```

### Step 1 — the first answer was wrong, and finding out why *was* the measurement

The raw count said zero: 64 re-executions from `go-ora`, none naming an unknown
cursor. That would have licensed step 3 immediately. It was wrong.

The ordered trace (`CURSOR_TRACE=1`) showed cursor-id learning stopping dead
part-way through the churn loop — parses 37, 38, 39 and the re-prepared
`SELECT 1 AS n FROM dual` learned nothing — while the *miss rate stayed at zero*.
Oracle recycles cursor ids, so each of those re-executions found a **stale**
tracker entry and resolved to the statement that last held the id. Five runs of
`SELECT 1 AS n FROM dual` were gated, and recorded in `/queries`, as
`SELECT 35 AS churn FROM dual`.

So the pre-existing behaviour was not "forwards ungated when it cannot resolve";
it was "enforces the wrong statement, silently". That is worse than the gap the
spec set out to close, and it was invisible to the metric the spec proposed.

### Step 2 — the cause, and why the proposed anchor was the wrong fix

`findCursorIDInResponse` bounded the OER's sequence number at 255. The constant's
comment said TTC numbers calls with a byte that wraps. It does not: that field is
the end-to-end ECID sequence — `SummaryObject.EndToEndECIDSequence` in go-ora, a
**uint16** counting up across the whole session. Consecutive captured responses
either side of the boundary (now pinned as unit fixtures in `ttc_oer_test.go`):

| Statement | OER sequence | Cursor id | Learned before the fix? |
|-----------|--------------|-----------|--------------------------|
| `SELECT 36 AS churn FROM dual` | 252 | 6 | yes |
| `SELECT 37 AS churn FROM dual` | 256 | 13 | **no** |

The bound is now the field's real width (`0xFFFF`), which is also what
`oerFieldMaxSizes` already allows.

Note what this says about the hardening the spec sketched: **correlating the
OER's sequence number with the TTC sequence byte of the request would have made
things worse.** The field is not a per-call sequence at all, so the correlation
has no basis; the existing over-tight reading of that same field is precisely
what was breaking learning. The lesson is the one from the Phase 1 `user_id_len`
bug — an anchored scan over an Oracle wire structure fails in both directions,
and tightening one is as dangerous as loosening it.

### Step 3 — the numbers that licensed failing closed

After the fix, two real thin clients:

| Client | Parses seen | Cursor ids learned | Re-executions | Naming an unknown cursor |
|--------|-------------|--------------------|---------------|--------------------------|
| `go-ora` v3 | 57 | 53 | 64 | **0** |
| `python-oracledb` thin 3.4.2 | 58 | 55 | 60 | **0** |

124 live re-executions, zero unresolved. The parses that learned nothing are
**exactly** the four statements that failed outright (a `DROP TABLE` on a missing
table and three retries of a `SELECT` on a missing table); their OER carries a
real ORA code, which the scan deliberately refuses to read an id from, and no
client re-executes a statement that errored. The test now asserts that list
exactly, so a learning regression fails loudly even when a recycled cursor id
would hide it.

Before the fix the same run learned 49 of 57 — and, again, reported zero misses.

Plus the always-on half: the four recorded captures in
`internal/proxy/oracle/testdata/*_cursor_reexec.pcapng` (go-ora SELECT, go-ora
INSERT, python-oracledb thin, JDBC thin) replay 12 more re-executions through
the real intercept paths, all resolved.

The interleaved-cursor triple came out 4/4/4, which is the mis-learning check
the miss count cannot make: a cursor id read off the wrong OER shows up as a
skew there long before it shows up as an unknown statement.

### What this did not close

The masking mechanism itself. Stale tracker entries still accumulate for the
life of a session — `handleOCLOSE` removes one cursor id per frame while clients
batch their closes — so a future learning miss on a recycled id would re-arm the
same silent wrong-SQL gate, without ever reaching `refuseUnknownCursor`. Filed
as `specs/todos/2026-08-10-04-oracle-stale-cursor-resolves-to-the-wrong-statement.md`
and listed as an open gap in `docs/approvals.md`.

The measurement's counters are guarded by
`TestCountingHandlerWatchesTheMessagesTheGateEmits` (untagged, runs in
`make test`). It exists because they rotted once already: routing the piggyback
frame through `refuseUnknownCursor` deleted the log line the untracked counter
watched, leaving the headline metric structurally unable to be non-zero while
still being cited as evidence. The log messages are now named constants in
`intercept.go`, so a reword moves the counter with it, and the guard proves an
untracked cursor really does emit a record the counter watches — on both sides
of the statement-controls branch.
