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
