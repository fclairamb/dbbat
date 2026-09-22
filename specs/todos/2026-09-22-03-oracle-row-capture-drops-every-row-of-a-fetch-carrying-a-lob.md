---
model: opus
effort: medium
---

# A CLOB or an XMLTYPE anywhere in a select list drops every row of that fetch

## Goal

Capture the rows of an Oracle fetch whose select list carries a LOB locator or
an XMLTYPE, instead of capturing none of them.

## Why

Found on 2026-09-22 while implementing
`2026-09-21-01-oracle-wide64-column-record-layout.md`, and measured rather than
reasoned about. `ociDescribeTypedQuery` was first written as five more columns
on `ociDescribeQuery`; with `TO_CLOB('cl')` and `XMLTYPE('<a/>')` in the same
select list, the row capture of that describe's fetch came back **empty** —
where the same eight-column query without them yields its row.

The control is in the tree. Over `testdata/oci_describe.hex` (the 4-byte
dialect, which captures rows correctly today):

- frame 1, `ociDescribeQuery`: 8 columns, **1 row**;
- frame 2, `ociDescribeTypedQuery`: 6 columns, **0 rows**.

Same session, same server, same encoding. The difference is the two columns.
That is why the two queries are kept apart in the capture harness — folding them
together would have cost `TestOCIRowCaptureCarriesTheDescribesColumnNames` its
row and hidden this behind a green test.

The column *names* and types are read correctly in both frames, so this is
`scanRowValues`, not the describe. The likely cause is shape rather than
absence: a LOB column's value on the wire is a **locator** (a fixed-size
structure naming the LOB, not its contents) and an XMLTYPE's is an opaque
value, neither of which is the length-prefixed scalar the scanner walks — so the
scan drifts at the first one and abandons the whole row, taking the ordinary
columns beside it with it.

Impact: a query joining an ordinary table to anything with a CLOB column — a
notes field, a document body, a JSON blob stored as CLOB — captures **no rows
at all**, not merely an unreadable value for that one column. The rest of the
row is lost with it, and nothing in the audit trail says a row was dropped.

## Implementation

1. Reproduce from the fixture, not from a live server: `testdata/oci_describe.hex`
   frame 2 is the failing payload and frame 1 the control. A test that pins "0
   rows" today is the wrong shape — pin the columns that *should* come back.
2. Find where `scanRowValues` (`internal/proxy/oracle/ttc_decode.go`) gives up.
   It already takes the per-column type codes from the describe, so it can tell
   a LOB (112/113/114) and an opaque type (`tnsTypeOPAQUE`, 58) apart from a
   scalar before it reads one.
3. Decide what a LOB column's captured value should be, and write the decision
   down. A locator is not the LOB's contents and dbbat must not go fetch them —
   that would be dbbat issuing statements of its own on the session's behalf.
   The honest options are a placeholder marking the type, or omitting that one
   key. **Whichever is chosen, the other columns of the row must survive**, which
   is the whole point of this spec.
4. The same treatment presumably applies to BLOB, BFILE and NCLOB; the fixture
   only holds a CLOB and an XMLTYPE, so extend `ociDescribeTypedQuery` and
   re-record if the others need pinning rather than assuming they behave alike.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `scanRowValues`
- `internal/proxy/oracle/describe_wide_test.go` — `TestOCIRowCaptureCarriesTheDescribesColumnNames`
- `internal/proxy/oracle/oci_fixture_capture_test.go` — `ociDescribeTypedQuery`
- `internal/proxy/oracle/testdata/oci_describe.hex` — the evidence
