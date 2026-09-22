---
model: opus
effort: medium
---

# A LONG column is read as a scalar and probably costs every row of the fetch

## Goal

Find out whether a genuine `LONG` / `LONG RAW` column's row value carries the
indicator-and-return-code trailer an *inlined LOB* does, and if it does, read it
— rather than reading the value and then walking into the trailer as if it were
the next column.

## Why

Found on 2026-09-22 while implementing
`2026-09-22-09-oracle-a-thin-client-that-asks-for-lob-locators-loses-its-rows.md`.

That spec established what the thin dialect's "inlined LOB" shape actually is,
and the finding is not about LOBs at all. go-ora's default LOB policy works by
**re-declaring the column as a LONG** in its define block — `CLOB` becomes
`LONG VARCHAR` (94), `BLOB` becomes `LONG RAW` (24) — and what comes back is
therefore an ordinary LONG column:

```
value CLR · indicator (compressed) · return code (compressed)
```

The NULL case spells those two as `-1` and `1405`, i.e. ORA-01403 "fetched
column value is NULL", which is what identifies them. `readCompressedInlineLOBColumn`
reads exactly that.

But `rowValueShapeOf` classifies a column by the **describe's** type code, and a
real `LONG` (8) or `LONG RAW` (24) column is `rowValueScalar` there. So dbbat
reads its value and then starts the next column on the indicator byte — the
columns after it drift, `rowEndsAtMarker` refuses the row, and the fetch
captures nothing. That is the same silent "no rows" the LOB reading exists to
end, one type family over, and it has had no recording in `testdata/` to show it.

It is a *probable* bug rather than a measured one: nothing in the corpus selects
a `LONG` column, so the claim "a LONG column carries the same trailer an inlined
LOB does" is an inference from go-ora's substitution rather than an observation
of a column Oracle described as `LONG`. Measuring it is step 1.

## Implementation

1. **Record it.** Add a capture alongside `TestCapture_GoOraLOBInline`
   (`internal/proxy/oracle/capture_lob_test.go`) that creates a table with a
   `LONG` column and one with a `LONG RAW` column — a table is required, Oracle
   allows at most one `LONG` per table and no `LONG` in a `SELECT ... FROM dual`
   expression list — and selects it between ordinary `CHAR` columns, the same
   alternation `goOraLOBQuery` uses so a drift is visible as a lost row. Record
   it on go-ora **and** python-oracledb thin: the two disagree about LOB
   framing, so they may disagree here.
2. **Check today's behaviour** with `replayCapturedRows`. If the fetch captures
   nothing, the inference is confirmed.
3. **Read it.** `rowValueShapeOf` gains a `rowValueLongInline` case for
   `tnsTypeLONG` and `tnsTypeLONGRAW`, and `readRowColumn` routes it to
   `readCompressedInlineLOBColumn` (which should then lose its LOB-specific
   name). Careful: the OCI dialects must be checked separately — a LONG column
   there may be framed differently again, exactly as its LOB columns are, and if
   there is no recording the OCI path keeps today's reading rather than
   inheriting the thin one.
4. **Pin both dialects** and keep the mutual-exclusion gate: a LONG fetch read
   under the scalar shape must come back with no rows, and vice versa, the way
   `TestThinLOBFetchIsNotOfferedTheOtherReading` does it.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `rowValueShapeOf`, `readRowColumn`,
  `readCompressedInlineLOBColumn`
- `internal/proxy/oracle/capture_lob_test.go` — the recording harness
- `internal/proxy/oracle/lob_dialects_test.go` — the pattern to mirror
- `docs/oracle.md` — "LOB, opaque and object columns do not send a
  length-prefixed datum", which would gain a fourth family
