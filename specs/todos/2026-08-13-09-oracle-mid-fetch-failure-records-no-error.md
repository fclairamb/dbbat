# A failure raised mid-fetch still records no ORA error

## Goal

A statement that starts streaming rows and *then* fails — ORA-01555 snapshot
too old, ORA-01722 on a row conversion, an ORA-03113 on a long fetch — should
record its `ORA-NNNNN` text in `queries.error`, the way a failure raised before
the first row now does.

## Why

`2026-08-13-02` measured where failures actually arrive: a standalone TTC func
`0x04` after a marker exchange, and — on both python-oracledb thin and go-ora —
usually **without** the end-of-call bit. `handleOERStatus` now reads those,
through `decodeErrorOER`, which proves the bytes are a diagnostic naming the
very code the OER's fields reported.

It runs only when `rowStreamActive()` is false, and that is deliberate: mid-
fetch the payload *is* row bytes, and a `0x04` run inside it is a value's length
prefix. That guard is the load-bearing constraint inherited from the production
incident where misread row data was persisted as `Query.Error`.

The cost of the guard was checked against the six failures in
`testdata/{python_thin,go_ora}_failed_stmt.pcapng` and found to be zero — the
server sends the OER *instead of* the QueryResult, so no measured failure has
columns decoded when it arrives, the divide-by-zero included. But every one of
those failures is raised before the first row. **A failure raised after rows
have started flowing was never measured**, and if one arrives as a bit-less
standalone `0x04` it is still dropped today: the statement stays pending and the
next statement's `flushPendingQuery` closes it as a success.

## Implementation

1. **Measure first, again.** Force a mid-fetch failure through the proxy and
   capture it. The reproducible one is ORA-01555: open a cursor over a large
   table with a small array size, commit enough churn from a second session to
   roll the undo out from under it, then keep fetching. ORA-01722 on a
   `TO_NUMBER` of a bad row late in a table is cheaper to arrange and may be
   enough — check whether it comes back as a standalone `0x04` at all, or is
   folded into the row stream. Add the capture next to the two above and extend
   `failed_stmt_replay_test.go`.
2. If it does arrive as a bit-less standalone `0x04` mid-stream, the relaxation
   cannot simply drop the `rowStreamActive()` guard — that is the exact shape a
   run of row bytes takes. The candidate is a *narrower* opening: accept it only
   when the payload is a **whole TNS Data packet whose TTC payload starts at
   byte 0 with the OER** (which is how `handleOERStatus` is routed) **and**
   `decodeErrorOER` proves the diagnostic, and record whether any capture
   fixture's row stream ever produces a packet that satisfies both. There is a
   ready-made corpus for that: `TestDumpReplay_*` already walks every capture in
   `testdata/`, so the false-positive rate can be measured rather than argued.
3. If nothing can be made safe, document the gap in `docs/oracle.md` next to the
   which-path-requires-the-bit table instead of leaving it implicit, and close
   this out — a measured, documented gap is a legitimate outcome.

No GitHub issue filed (automation does not run `gh issue create` — see
`specs/todos/2026-08-11-06-*.md`); one should be filed by hand.
