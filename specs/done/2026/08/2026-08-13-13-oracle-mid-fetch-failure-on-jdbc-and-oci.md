# A mid-fetch failure is measured on two clients out of four

## Goal

Record a mid-fetch failure on **JDBC thin** and on an **OCI** client (sqlplus /
Instant Client), and confirm the two anchors `handleOERStatus` now relies on
mid-row-stream hold there: the failure arrives as a standalone TTC func `0x04`
at byte 0 of a whole TNS Data packet, and its `cursorID` field names the cursor
whose rows are streaming.

## Why

`2026-08-13-09` measured that a failure raised **after** rows start flowing does
arrive as a bit-less standalone `0x04`, and relaxed `handleOERStatus` to record
its ORA text. Mid-stream that relaxation is anchored twice: `decodeErrorOER`
proving the diagnostic, and `midFetchOERNamesTheStreamingCursor` requiring the
OER to name the streaming cursor — measured to hold exactly on python-oracledb
thin (cursor 4) and go-ora (cursor 3).

The anchor **fails closed**: a mid-fetch OER naming a different cursor is
dropped and the statement is recorded with no error, which is the pre-fix
behaviour. So a client whose OER reports a different id (or none) silently keeps
the old bug. The failing case is invisible except for one DEBUG line, and JDBC
thin and the OCI clients — both of which take other paths through dbbat's cursor
learning — are exactly the ones not covered.

There is also no *live* end-to-end test of the mid-fetch case: the failure is
replayed out of the two captures, but `failed_stmt_integration_test.go` (which
drives a real Oracle through the whole proxy) still only covers failures raised
before the first row.

## Implementation

1. Extend `internal/proxy/oracle/capture_midfetch_fail_test.go` (build tag
   `capture`) with a JDBC thin and an OCI variant of the existing fixture — the
   same `TO_NUMBER` over a 20 000-row table whose row 15 000 will not convert,
   no `ORDER BY`, fetch size 100. The OCI harness and the ojdbc jar path are
   described in `docs/oracle.md` and in the memory notes on the Oracle test
   harness; model the drivers on `capture_reexec_test.go`, which already records
   `sqlplus_cursor_reexec.pcapng` and `jdbc_thin_cursor_reexec.pcapng`.
2. Feed the new captures into `midFetchDumps()` in
   `internal/proxy/oracle/midfetch_fail_replay_test.go`. All four assertions
   should hold unchanged, `TestDumpReplay_MidFetchFailureIsABitLessStandaloneOER`
   included — that test already asserts the OER names the streaming cursor, so a
   client that disagrees fails it loudly instead of degrading quietly.
   `TestDumpReplay_MidStreamOERFalsePositiveRate` picks the new files up on its
   own (it globs `testdata/*.pcapng`) and its accepted-count assertion must be
   widened from 2 to the number of genuine mid-fetch fixtures.
3. Add a mid-fetch case to `failed_stmt_integration_test.go`: build the table,
   run the failing SELECT through the proxy against a live Oracle, and assert
   the persisted `queries.error` carries `ORA-01722`.
4. If a client turns out **not** to name the streaming cursor, do not weaken the
   anchor by reflex — record what it does send and decide against that, the way
   `2026-08-13-09` decided against its own measurement. Update the
   "A failure raised mid-fetch" section of `docs/oracle.md` either way.

No GitHub issue filed (automation does not run `gh issue create` — see
`specs/todos/2026-08-11-06-*.md`); one should be filed by hand.
