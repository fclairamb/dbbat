---
model: opus
effort: high
---

# A `SYS_REFCURSOR` fetch is refused under a restrictive grant, because its cursor id is never learned

## Goal

Learn the cursor id a stored procedure's `OUT SYS_REFCURSOR` hands back, so that
driving a REF cursor stops being an unidentifiable execution — and stops
becoming `ORA-01031` the moment the session's grant carries `read_only`,
`block_ddl` or an approval pattern.

## Why

Found on 2026-09-19 while fixing
`2026-09-19-04-cursor-id-learning-misses-five-of-sixty-nine.md`. The five
unknown-cursor re-executions that test was reporting turned out to be exactly
the five drives of the REF cursor in its own workload, on both thin clients,
with a different id each time.

It is structural, not a locator bug. `learnCursorID` reads the id off the
response to a statement dbbat **saw parsed**, and a REF cursor has no such
statement on the wire:

```sql
PROCEDURE dbbat_learn_refcur(p OUT SYS_REFCURSOR) AS
BEGIN
  OPEN p FOR SELECT LEVEL AS n FROM dual CONNECT BY LEVEL <= 5;
END;
```

`OPEN p FOR …` runs inside the procedure body. The server allots the cursor
while executing `BEGIN dbbat_learn_refcur(:1); END;` and reports its id back in
that call's **out-bind data**, not in an OER `findCursorIDInResponse` scans. So
the client then fetches from an id the tracker holds no entry for, and
`refuseUnknownCursor` does what it does for every unidentifiable execution:
forwards it with a WARN under a permissive grant, **refuses it** under a
restrictive one.

That refusal is correct as a rule and wrong as an outcome here. A REF cursor is
ordinary, read-only application code — it is how PL/SQL returns a result set —
and today a `read_only` grant, which is the *most* common restrictive grant,
turns every one of them into `ORA-01031`. The measurement now names this as the
one known case where ordinary work is refused (`docs/oracle.md`, "The one cursor
id that cannot be learned"), which is honest but not a fix.

## Implementation

1. Decode the out-bind. The call's response carries the bind outputs; a
   `SYS_REFCURSOR` out-bind is the cursor id. Find it in the corpus first —
   record a REF-cursor session with the capture harness
   (`internal/proxy/oracle/capture_*_test.go`) for go-ora, python-oracledb thin
   and, if it will run, OCI — before writing any decoder. The id must be
   located **to the byte**, under bounds as tight as
   `findPlausibleOERInResponse`'s: a loose scan here plants a wrong entry in the
   tracker, which is worse than no entry (see the stale-entry incident in
   `docs/oracle.md`, "Learning the cursor id").
2. Decide what SQL the learned cursor carries. The `SELECT` inside the procedure
   is not on the wire, so the only text dbbat has is the **call** — which is
   also the statement the grant already gated once, and the right thing to
   charge quota and `/queries` to. Whatever is chosen has to be visible as such:
   a `/queries` row that claims to be the SELECT would be a lie.
3. Gate it. Once the id is in the tracker, `regateCursor` needs no change — the
   fetch resolves and is re-gated against the call exactly like any other
   re-execution.
4. Bound the tracker. A procedure called in a loop opens a fresh REF cursor per
   call; the ids recycle, which `logMsgRecycledCursorID` already reports, but
   confirm the peak stays under `trackerPeakBound` on a REF-cursor-heavy
   workload.
5. Flip the measurement. `TestIntegration_CursorIDLearningMissRate` and its
   PythonThin twin currently *require* each REF-cursor drive to name an unknown
   cursor (`refCursorDrives == untrackedDuringRefCursor`). When this lands, that
   assertion inverts: the drives resolve, the split scripts can be rejoined, and
   the exemption paragraph in `docs/oracle.md` and the note in
   `docs/approvals.md` come out.
6. Add the case the gap actually costs: a REF cursor driven under a `read_only`
   grant must complete, alongside
   `TestIntegration_CursorReexecUnderReadOnlyIsNotBrokenByTheGate`. That test is
   the one that would have caught this from the user's side, and it does not
   exercise a REF cursor today.

## Files

- `internal/proxy/oracle/intercept.go` — `learnCursorID`, `refuseUnknownCursor`
- `internal/proxy/oracle/ttc_oer.go` — `findCursorIDInResponse` and the bind decoding next to it
- `internal/proxy/oracle/cursor_learning_integration_test.go` — both measurements
- `docs/oracle.md` — "The one cursor id that cannot be learned: a REF cursor"
- `docs/approvals.md` — the untracked-cursor bullet

## Implementation Plan

### What the recordings say

Four fixtures were recorded against Oracle Free 23ai before a line of decoder
was written (`internal/proxy/oracle/capture_refcursor_test.go`):
`go_ora_refcursor.pcapng`, `python_thin_refcursor.pcapng`,
`jdbc_thin_refcursor.pcapng`, `sqlplus_refcursor.pcapng`. All four call the same
procedure three times, so the server hands out a **fresh** id per `OPEN` and a
locator that latched onto the first would be caught.

The id is not in an OER. It is the last field of a **REF cursor descriptor**
inside the call's bind-output message (TTC message type `0x07`), and that
descriptor is byte-for-byte the describe body dbbat already parses
(`describe.go`), plus a trailing cursor id — `RefCursor.load` and `case 16:` in
go-ora's `command.go` are the same field list:

```
[0x07]                      bind-output message
  len        byte
  maxRowSize cint
  colCount   cint
  [1 byte]   colCount x column-describe record   (parseColumnDescribe)
  dlc
  cint cint          TTCVersion >= 3
  cint cint          TTCVersion >= 4
  dlc                TTCVersion >= 5
  cursorID   cint    <- the REF cursor
```

On the **first** call the `0x07` message is preceded by the IO-vector message
`0x0b` (bind directions); on a re-execution it is the payload's leading byte.
Both were walked by hand and the id checked against the very next client frame —
the drive's `03 5e … 02 80 50 01 <id>` — for every recording: go-ora 2 then 7,
python-thin 4, jdbc-thin 4. The OER of the same response names a *different*
cursor (6 for go-ora's second call): that one is the call's own, which
`learnCursorID` already reads.

sqlplus marshals the identical field list in the **wide/fixed-width** OCI
encoding (4-byte little-endian instead of compressed ints), which the compressed
walk refuses at its first field. That stays a documented gap rather than a
guessed second decoder.

### Steps

1. `internal/proxy/oracle/refcursor_bind.go` — `refCursorIDsInBindOutput`, a
   deterministic walk (never a scan): the `0x0b` IO vector if present, then the
   `0x07` body, then one descriptor per out-bind, reusing `dcursor` and
   `parseColumnDescribe` (classic tail first, then modern, exactly as
   `parseColumnDescribes` does). Bounds: every column type must be a known
   TNSType, the id must be a plausible 16-bit cursor, and the walk must **land**
   on a terminator (end of payload, or an OER `0x04` / Response `0x08` message
   byte, optionally past the one trailing cint PL/SQL appends). A walk that does
   not land cleanly learns nothing.
2. `refcursor_bind_test.go` — replay all four fixtures, asserting the exact ids
   against the ids the next client frame drives, and asserting the OCI recording
   yields none.
3. `intercept.go` — `learnRefCursorIDs`, called from `interceptUpstreamMessage`
   next to `learnCursorID`. It runs only while the pending statement is a PL/SQL
   call (`BEGIN`/`DECLARE`/`CALL`) that is not itself a REF-cursor entry, and
   only until it has learned something for that call.
4. The learned cursor carries the **call**'s SQL plus `refCursorNote` — a SQL
   comment, like `partialStatementNote`, so the validators and approval patterns
   match what they would have matched anyway while `/queries` stops implying the
   inner `SELECT` was on the wire.
5. `regateCursor` is untouched: the entry is an ordinary tracker entry, so a
   drive resolves and is re-gated against the call.
6. Tests: invert `refCursorDrives == untrackedDuringRefCursor` in both
   `TestIntegration_CursorIDLearningMissRate` measurements, rejoin the split
   python script, add a REF cursor under `read_only` next to
   `TestIntegration_CursorReexecUnderReadOnlyIsNotBrokenByTheGate`, and confirm
   `trackerPeakBound` still holds. `docs/oracle.md` and `docs/approvals.md` lose
   the "cannot be learned" language.
