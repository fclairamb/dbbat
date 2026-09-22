---
model: opus
effort: medium
---

# The 64-bit OCI dialect now reads its column names and still captures no row values

## Goal

Make `scanRowValues` read the row data a 64-bit OCI session's fetch carries, so
that dialect's `query_rows` hold values rather than nothing. The names are
already right; the values are the other half.

## Why

Found on 2026-09-22 while implementing
`2026-09-21-01-oracle-wide64-column-record-layout.md`. That change made
`parseColumnDescribes` read the 64-bit dialect's describe records, which is what
`decodeQueryResultV2` takes its column names and types from. Measured
immediately afterwards, over both describe fixtures, one frame per row:

| fixture | frame | columns | rows |
|---|---|---|---|
| `testdata/oci_describe.hex` (4-byte) | login probe | 1 | **1** |
| `testdata/oci_describe.hex` | `ociDescribeQuery` | 8 | **1** |
| `testdata/oci_describe.hex` | `ociDescribeTypedQuery` | 6 | 0 |
| `testdata/oci64_describe.hex` | login probe | 1 | 0 |
| `testdata/oci64_describe.hex` | `ociDescribeQuery` | 8 | 0 |
| `testdata/oci64_describe.hex` | `ociDescribeTypedQuery` | 6 | 0 |

The 4-byte column is the control: the *same query*, against the *same server*,
in the *same session shape*, yields a row there and none here. So this is not
"that describe carries no row data" — it does, visibly: the values `07 02 c1 02`
(the NUMBER 1), `01 78` (`'x'`) and the rest sit in `oci64_describe.hex` frame 1
at roughly offset 0x360, right where the 4-byte fixture's do. `scanRowValues` is
simply not finding them, because it is a heuristic scanner written against the
compressed/4-byte value stream and never revisited for this dialect.

The visible consequence is the one
`2026-09-20-02-oracle-oci-row-capture-still-scans-for-column-names.md` describes
for names, one layer down: a 64-bit OCI session's captured rows are empty JSON
objects. The names being right now makes that *more* visible, not less.

**It is also why `TestIntegration_OCIRowCaptureCarriesRealColumnNames` fails
under the container client**, and that was checked rather than assumed. Run with
`ORACLE_TEST_OCI_CLIENT=container` (the 23.26 sqlplus bundled in the image, which
speaks the 64-bit dialect), it stops at `no captured row for a statement
containing "UPPER('x')"` — before it ever reaches the column-name assertion it
exists for. Restoring the pre-change decode input at the one call site
(`decodeQueryResultV2(ttcPayload, oerShape{fixedWidth: …})`, i.e. the reading
that was in place before the describe records were readable at all) and running
the same test against the same image fails **identically**, same message, 77s vs
79s. So the failure is this defect, not the describe work: it is the row scan,
and it predates the records being read. With a 4-byte Instant Client on PATH the
same test passes.

(The third line of the table is a different defect and has its own spec:
`2026-09-22-03-oracle-row-capture-drops-every-row-of-a-fetch-carrying-a-lob.md`.)

## Implementation

1. Start from the evidence already in the tree — `testdata/oci64_describe.hex`
   frames 1 and 2 — and diff the value region against the 4-byte fixture's, the
   same way the column record was pinned: from **non-zero values**, never from
   runs of zeros. The two fixtures are the same two queries against the same
   server, so every value has a known counterpart.
2. `scanRowValues` in `internal/proxy/oracle/ttc_decode.go` takes the column
   types but no encoding; it will need the session's `oerShape` the way
   `parseColumnDescribes` now does. Thread it from `decodeQueryResultV2`, which
   already has it.
3. Hold it to the bar `TestOCIRowCaptureCarriesTheDescribesColumnNames` sets for
   the 4-byte dialect: the *values*, keyed by the describe's own names, out of
   `oci64_describe.hex` frame 1 — `N2` = 1, `BIG` = `x`, `FLT` =
   0.3333…, `C5` = `ab   `, `R` = `7a7a`, and the two temporal columns at the
   recording's own timestamps.
4. If the value stream turns out to need evidence the corpus does not hold, say
   so in the spec's place rather than fitting a layout to one sample — the rule
   the column-record spec was closed under.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `scanRowValues`, `decodeQueryResultV2`
- `internal/proxy/oracle/describe_wide64_test.go` — where the assertion belongs
- `internal/proxy/oracle/testdata/oci64_describe.hex` — the evidence
