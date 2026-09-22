---
model: opus
effort: medium
---

# The LOB and object framing lengths are measured on the 64-bit OCI dialect and nowhere else

## Goal

Record a LOB-carrying fetch from a **thin** client (go-ora / JDBC thin,
compressed encoding) and from the **4-byte** OCI dialect, and pin the row walk's
framing skips against them — or find out that they differ and branch the reading
the way `rowDataStart` already branches.

## Why

Found on 2026-09-22 while implementing
`2026-09-22-03-oracle-row-capture-drops-every-row-of-a-fetch-carrying-a-lob.md`.

That spec taught `readRowColumn` that a LOB locator is followed by a fixed block
of framing (`lobLocatorTrailerLen` = 16, `lobNullTrailerLen` = 3) and that an
opaque or object locator is followed by framing plus the object's own image. All
of it is measured off **one** recording, `testdata/oci64_lob.hex`, taken from the
sqlplus bundled in `gvenzl/oracle-free:23-slim` — the 64-bit OCI dialect.

The object *image header* is read rather than measured, so it already spans both
OCI dialects (12 bytes of framing ahead of it on the 64-bit one, 14 on the
4-byte one, both found by the same search). The two **LOB** constants are not:
nothing in the corpus says a thin client's fetch spells them the same way, and
the analogous object framing is already known to differ by two bytes between the
dialects — which is exactly the kind of difference that would make 16 wrong.

It is not a live correctness bug, and that is the reason it is a follow-up
rather than part of the spec above: a wrong skip drifts the columns behind it,
`rowEndsAtMarker` then refuses the row, and the capture is the "no rows" it
already was. So the cost is a silently unfixed dialect, not a wrong row. But
"silently unfixed" is what the original defect looked like too.

## Implementation

1. Record the thin-client half. `ociLOBQuery` already exists
   (`oci_fixture_capture_test.go`) and the `capture`-tagged harness can drive
   go-ora directly; the compressed dialect's fetch may or may not be deferred
   the way the OCI one is, and finding that out is half the value of the
   recording.
2. Record the 4-byte OCI half. It needs an Instant Client on PATH
   (`ORACLE_TEST_OCI_CLIENT=path`), which is how `testdata/oci_describe.hex` was
   taken — `writeLOBFetchHexFixture` already writes to `ociLOBFixture`
   (`testdata/oci_lob.hex`) when the recorded dialect is not the 64-bit one, so
   the capture route is in place and unused.
3. Pin each with its own copy of `TestOCI64LOBFetchKeepsEveryOrdinaryColumn`,
   and its own `…IsNotOfferedToTheOtherTwoEncodings` gate.
4. If a dialect's trailer differs, the lengths become per-shape the way
   `rowDataStart` is, rather than the two package-level constants they are now.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `lobLocatorTrailerLen`,
  `lobNullTrailerLen`, `readRowColumn`
- `internal/proxy/oracle/oci_fixture_capture_test.go` — `ociLOBQuery`,
  `writeLOBFetchHexFixture`, `ociLOBFixture`
- `internal/proxy/oracle/lob_rowcapture_test.go` — the tests to mirror
- `internal/proxy/oracle/testdata/oci64_lob.hex` — the one recording there is
