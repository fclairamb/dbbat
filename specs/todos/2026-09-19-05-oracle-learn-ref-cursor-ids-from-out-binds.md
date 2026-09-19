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
