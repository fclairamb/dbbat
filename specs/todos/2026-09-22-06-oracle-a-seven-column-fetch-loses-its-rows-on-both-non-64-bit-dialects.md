---
model: opus
effort: medium
---

# A seven-column fetch's row area is found three bytes early, on both of the dialects that scan for it

## Goal

Stop `findRowDataStart` from mistaking a column *count* for the `0x07` ROW_DATA
message byte, so a query with exactly seven columns captures its rows on the
compressed and 4-byte OCI dialects the way it already does on the 64-bit one.

## Why

Found on 2026-09-22 while implementing
`2026-09-22-02-oracle-wide64-row-values-are-never-scanned.md`, which gave the
64-bit dialect a structural row-header reading and left the other two on the
scan that was already there.

`findRowDataStart` locates the ROW_HEADER by the byte pair `06 22`, then scans
**forward from the very next byte** for the first `0x07` and calls what follows
it the first row. The very next byte is the start of the header's column-count
field, which on the 4-byte OCI dialect is a little-endian ub4:

```
06 22 | 07 00 00 00 | 00 00 01 00 | <12 bytes> | 07 | <first value>
        ^^ a seven-column query's count
```

So a seven-column fetch returns `idx+3` instead of `idx+22`, and
`parseRowStream` starts reading lengths out of the middle of the header rather
than at the first value. It will not produce the row; it will produce whatever
the header's remaining bytes look like, or nothing.

The corpus does not contain a seven-column fetch — the recorded describes carry
one, eight and six columns — which is why no test catches it and why this is a
spec rather than a fix made in passing. It is not specific to the fixed-width
encoding: the compressed dialect writes the count as a TTC compressed int, where
7 is likewise the single byte `0x07`.

The 64-bit dialect is already immune: `findRowDataStartWide64` does not scan. It
requires `0x06` at +0, the `0x22` flag at +2, the describe's own column count as
a ub4 at +4, and the ROW_DATA byte at exactly +50 — so there is no forward scan
to trip, and a mismatch yields no rows rather than wrong ones.

## Implementation

1. Give the other two dialects the same treatment, which is the point: the
   header's length is **measured**, not searched for. The 4-byte one is pinned
   by both frames of `testdata/oci_describe.hex` that carry a header (column
   counts 1 and 8) at a total of 22 bytes:
   `0x06`, `0x22` flag, ub4 count, ub4 `0x00010000`, 3 × ub4 = 0, then `0x07`.
   See the table under "The 64-bit dialect's rows are behind a wider ROW_HEADER"
   in `docs/oracle.md`, which sets the two side by side.
2. The compressed dialect's header is **not** measured anywhere yet and must be
   before it is changed — its integers are self-sizing, so the header has no
   fixed length and the reading has to walk the fields. `go_ora_*.pcapng` and
   the thin `python_thin*.pcapng` / `jdbc_thin_*.pcapng` recordings are where
   the samples are. If the walk cannot be pinned from them, leave that dialect
   on the scan and say so rather than fitting one.
3. Validate the count against the describe's, as the 64-bit reading does — it is
   the check that makes a wrong landing fail closed instead of producing a row
   of garbage.
4. A regression test needs a seven-column fetch, and the corpus has none. Either
   record one (`capture_oci_fixtures_integration_test.go` is the route the two
   describe fixtures came from — add a seven-column query to it), or synthesize
   the header bytes in a unit test from the layout above; the recorded one is
   worth more.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `findRowDataStart`, `rowDataStart`,
  `findRowDataStartWide64` (the shape to copy)
- `internal/proxy/oracle/testdata/oci_describe.hex` — the 4-byte header's two
  samples
- `docs/oracle.md` — "The 64-bit dialect's rows are behind a wider ROW_HEADER"
