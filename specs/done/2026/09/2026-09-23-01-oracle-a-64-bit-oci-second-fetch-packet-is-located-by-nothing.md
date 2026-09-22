---
model: opus
effort: medium
---

# A 64-bit OCI fetch after the first loses its whole packet

## Goal

Read the ROW_HEADER of a 64-bit OCI fetch packet whose flag byte is `0x02`
rather than `0x22`, so the rows of every round trip after the first are captured
instead of silently dropped.

## Why

Found on 2026-09-23 while implementing
`2026-09-22-10-oracle-a-long-column-is-read-as-a-scalar-and-costs-the-row.md`.

sqlplus fetches a result set over several round trips. The recording made for
that spec (`testdata/oci64_long.hex`) carries two of them for one query, and
their packets do not open the same way:

```
frame 2   06 01 22 ...   the first fetch   → located, rows captured
frame 3   06 01 02 ...   the next fetch    → located by nothing, rows lost
```

`wide64RowDataStartAt` demands `wide64RowHeaderFlag` (0x22) at +2, so the second
packet is refused, and `parseContinuationRows`'s fallback — scan the first 25
bytes for the ROW_DATA byte — cannot save it either, because the 64-bit header
is 50 bytes long and the `0x07` sits past the window.

The **4-byte** dialect has the same flag difference and does not lose the rows:
its header is 22 bytes, so the fallback scan finds the `0x07` at index 22.
That asymmetry is the whole of the defect — the values behind the header are
identical on both dialects, and `testdata/oci_long.hex` reads both of its rows.

This is not about LONG columns. Any 64-bit OCI fetch of more than one round trip
loses every packet after the first, whatever its columns are. It has gone
unnoticed because the corpus had no such fixture until now: every other OCI
recording returns its rows in a single packet.

## Implementation

1. **Measure the flag.** `testdata/oci64_long.hex` frames 2 and 3 are the pair.
   Establish what `0x02` means against `0x22` — `oci_long.hex` frames 2 and 3
   are the 4-byte counterparts of exactly the same two round trips, so the two
   dialects can be compared field by field rather than guessed at.
2. **Widen the reading, not the scan.** `wide64RowDataStartAt` should accept
   both flag values (or whichever bits of that byte are the invariant), keeping
   every other check it fails closed on: the column count must be the describe's
   and the ROW_DATA byte must land exactly where the header ends. Do **not**
   widen `parseContinuationRows`'s 25-byte fallback window — that scan is a
   heuristic the header readings exist to replace.
3. **Check the 4-byte reading too.** `wideRowDataStartAt` demands `0x22` at +1
   and is being rescued by the fallback scan rather than reading its own
   header. It should accept `0x02` for the same reason, so the packet is
   *located* rather than stumbled upon.
4. **Pin it.** `TestOCILongFetchKeepsEveryOrdinaryColumn` in
   `internal/proxy/oracle/long_dialects_test.go` already carries the expectation
   this would change: `oci64LongFrames` is pinned at `longRows[:1]`, with a
   comment naming this spec. It becomes `longRows`, the same as every other
   recording of the same query. Add the negative half as well — a header with a
   flag neither value must still be refused.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `wide64RowDataStartAt`,
  `wideRowDataStartAt`, `fetchRowDataStart`, `parseContinuationRows`
- `internal/proxy/oracle/long_dialects_test.go` — the pin that names this
- `internal/proxy/oracle/testdata/oci64_long.hex`,
  `internal/proxy/oracle/testdata/oci_long.hex` — the evidence
