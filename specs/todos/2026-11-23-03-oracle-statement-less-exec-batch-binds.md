# Oracle: a statement-less `03 5e` re-execution records one bind row only

## Goal

When a client re-executes a cursor through the full execute op with no
statement text (python-oracledb does when a bind outgrew its declared buffer,
ojdbc6 always does), every bind row of the call is recorded, as it already is
for a `03 5e` with a statement and for a `03 04` re-execution.

## Why

`handleCursorReexec` reads the bind data of such a frame with
`findBindRowsFromTail(frame, 0, count, 1)`: one row, the last. The iteration
count that would say how many rows to read is `al8i4[1]`, and
`execIterations` finds `al8i4` right behind the statement text, which this
frame does not have. An `executemany` that lands on this shape (a batch whose
values are longer than the previous batch's) is recorded with its last row and
no batch size, the under-reporting the array-DML fix removed elsewhere.

## Implementation

- Walk the thin exec header to its end (python-oracledb
  `_write_execute_message`: options, cursor id, the sql pointer and length, the
  al8i4 pointer and length 13, ... through the 12.2 oaccolid pointers) so
  `al8i4` can be located without a statement, and read `al8i4[1]` there.
- Reuse `findBindRowsFromTail(frame, lo, count, iterations)`.
- Add to `pythonArrayDMLScript` a second executemany whose values are longer
  than the first's, re-record, and assert its batch.
