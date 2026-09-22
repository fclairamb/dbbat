---
model: opus
effort: medium
---

# The 64-bit OCI column record is 25 bytes wider and nobody can say where those bytes are

## Goal

Pin the 64-bit OCI dialect's per-column describe record field by field, so
`parseColumnDescribe` can walk it and the two things that currently route
around it stop doing so: the REF-cursor bind-output walk
(`refcursor_bind_wide64.go`, which anchors the descriptor's tail instead of
parsing the columns) and `parseColumnDescribes`, which on that dialect
misaligns and returns nil.

## Why

Found on 2026-09-20 while implementing
`2026-09-20-03-oracle-wide64-oci-refcursor-and-drive.md`. Two recordings from
one live sqlplus 23.26 session through dbbat —
`testdata/oci64_refcursor_bind_output.hex` and `testdata/oci64_describe.hex`,
the latter describing a deliberately type-rich query — pin most of the record
exactly:

- type, flag, precision and scale are four bytes as in the 4-byte dialect;
- `maxLen` is a `ub4` right behind them (22 on a NUMBER, 4000 on a
  `VARCHAR2(4000)`, 2000 on the object column);
- the type OID's DLC length is a `ub4` too (16 on the object column), twelve
  bytes past `maxLen`;
- `charsetID` is a `ub2` (873), `maxCharLen` and `oaccollid` are `ub4`s (4000
  and 16382), `allowNull` and the v7 name length are single bytes, and the
  name/schema/type-name DLCs are `ub4` lengths plus a CLR — all exactly where
  the 4-byte dialect puts them relative to each other.

What is **not** pinned is where the extra 25 bytes per record sit. Measured as
the distance between consecutive records' `allowNull` bytes, every record is 25
bytes longer than the 4-byte dialect's — **except** the object column's, which
is 14 longer. Splitting that 11-byte difference needs a column that is
non-empty in exactly one of the three variable-length fields, and the corpus has
none: the object column is the only one with a type OID, and it is also the only
one with a schema and type name. Any placement consistent with the seven scalar
columns is equally consistent with the object one, so shipping a field walk
would mean shipping offsets no recording can falsify.

The consequences today are two, and neither is a correctness risk — both are
"reads nothing" rather than "reads wrong":

1. `refCursorIDsInBindOutputWide64` anchors the descriptor's trailing block on a
   signature (a seven-byte DLC carrying an Oracle DATE) and validates by landing,
   rather than walking the columns. It works, and a wrong layout there yields no
   id — but it is a scan where the rest of this package walks, and it cannot use
   `isKnownTNSType` as an alignment proof.
2. `parseColumnDescribes` is offered the 4-byte layout on a 64-bit session
   (`describeWireLayout` keys on `fixedWidth` alone), misaligns on the extra
   header byte before the first record, and returns nil — so row capture there
   falls back to `scanAndPadColumnNames`, the same gap
   `2026-09-20-02-oracle-oci-row-capture-still-scans-for-column-names.md`
   describes for the 4-byte dialect.

## Implementation

1. **Record the disambiguating columns first.** The whole blocker is missing
   evidence, so extend `ociDescribeQuery` (it is shared by both capture routes)
   with columns that separate the three effects:
   - a column with a type OID but a *different* type-name length, to see whether
     the extra bytes follow the OID or the names;
   - a `SYS.XMLTYPE` or `CLOB` column, which carries a type name without an
     ordinary object OID, if one exists on 23ai that does;
   - a column with a non-empty `toID` and a one-character name.
   Re-record with `ORACLE_CAPTURE_OCI_FIXTURES=1 ORACLE_TEST_OCI_CLIENT=container
   go test -tags integration -run TestCapture_OCIFixturesThroughDBBat`. If a
   query that separates them cannot be written, say so in the spec's place
   rather than fitting a layout to one sample.
2. Derive the field widths from the new fixture the way the ones above were
   derived — from non-zero values, never from runs of zeros — and give
   `dcursor`/`parseColumnDescribe` a `wide64` axis. Note two differences already
   measured and needing a home: `version` is **one** byte where the 4-byte
   dialect spends two, and `charsetForm` is **two** where it spends one.
3. Point `describeWireLayout` at a `describeColumnLayoutWide64` that accounts for
   the extra byte after the describe header's skip byte, and hold it to the same
   bar `TestOCIDescribeRecordsParse` sets: every name and every type of the
   eight-column describe, read out of `testdata/oci64_describe.hex`.
4. Only then replace the anchored tail in `refcursor_bind_wide64.go` with a real
   walk — and keep the cross-check it has (`TestDumpReplay_OCI64RefCursorIDs
   MatchTheCursorsTheClientDrives` must still read 3, 2, 3 and still agree with
   the drives) plus the false-positive fixture
   (`testdata/oci64_scalar_outbind_bind_output.hex`). A walk that reads a
   *different* id than the anchored version reads is a regression, not progress.
5. Update `docs/oracle.md`, "The 64-bit dialect reads the same field list, and
   not the same way", which currently records this as an open measurement.

## Files

- `internal/proxy/oracle/describe.go` — `dcursor`, `parseColumnDescribe`,
  `describeWireLayout`, `describeColumnLayoutWide`
- `internal/proxy/oracle/refcursor_bind_wide64.go` — the anchored tail this
  would replace
- `internal/proxy/oracle/oci_fixture_capture_test.go` — `ociDescribeQuery`
- `internal/proxy/oracle/capture_oci_fixtures_integration_test.go` — the
  recording route
- `internal/proxy/oracle/testdata/oci64_describe.hex` — the evidence, to be
  extended
- `docs/oracle.md`
