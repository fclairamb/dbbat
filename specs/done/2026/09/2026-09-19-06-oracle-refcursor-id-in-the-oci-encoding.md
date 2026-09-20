---
model: opus
effort: medium
---

# A REF cursor driven from sqlplus is still refused: its bind output is in the OCI encoding

## Goal

Read the `SYS_REFCURSOR` id out of a call's bind output when the session speaks
the wide/fixed-width OCI encoding, so a REF cursor driven from sqlplus, Instant
Client or SQL*Developer-over-OCI stops being `ORA-01031` under a `read_only`,
`block_ddl` or approval grant — the same fix
`2026-09-19-05-oracle-learn-ref-cursor-ids-from-out-binds.md` landed for the
three thin clients.

## Why

Found on 2026-09-19 while implementing that spec. Four clients were recorded
calling the same procedure; three of them are thin and all three now resolve.
sqlplus marshals the **identical field list** — descriptor length, max row size,
column count, the column-describe records, the version-gated trailing fields and
the cursor id — as four-byte little-endian integers instead of TTC compressed
ones, and `refCursorIDsInBindOutput` refuses it at its first field.

That refusal is the safe outcome and was deliberate: an OCI session keeps the
behaviour it had before, while a number read out of the wrong encoding would gate
a fetch against the wrong statement's grant and text. But it leaves the whole
thick-client family with the very gap the feature exists to close, and thick
clients are exactly who runs a `VARIABLE rc REFCURSOR` / `PRINT rc` session.

The bytes are in hand: `internal/proxy/oracle/testdata/oci_refcursor_bind_output.hex`
carries two real sqlplus call responses, and `TestCapture_SQLPlusRefCursor`
regenerates them.

Hand-walked, the first of them reads (payload offsets, after the two data-flag
bytes):

```
0b        IO vector
05        byte
01 00                    columnCount  (2-byte LE)
00 00 00 00              hi           (4-byte LE)
01 00 00 00              rowCount
00 00                    uACBufferLength
00 00 00 00              dlc length
00 00 00 00              dlc length
10        one direction byte per bind: 16 = Output
07        bind output
  4c                     descriptor length
  42 00 00 00            maxRowSize   (4-byte LE)
  02 00 00 00            colCount
  82                     the marker byte
  02 …                   first column record: NUMBER
```

i.e. every `cint` of the thin layout is a fixed-width little-endian field whose
*width* is the `size` argument of go-ora's `GetInt(size, …)` at that call site —
2 for the column count and the UAC buffer length, 4 for everything else.

## Implementation

1. Give `dcursor` a wide mode, or add a sibling cursor, that reads fixed-width
   little-endian fields of a stated width instead of compressed integers. The
   per-field widths have to come from the call site (go-ora's `GetInt(2|4, …)`),
   so the existing width-less `cint()` cannot simply be reinterpreted — this is
   the part to get right, and `testdata/oci_refcursor_bind_output.hex` is the
   check.
2. The column-describe record (`parseColumnDescribe`) needs the same treatment,
   which is the bulk of the work and is **useful beyond this spec**:
   `parseColumnDescribes` is compressed-only too, so an OCI session currently
   falls back to heuristic column-name scanning for row capture.
3. Offer the wide walk only when the session has learned it speaks that encoding
   (`oerShape.fixedWidth`), never by sniffing — the same rule
   `decodeOERFixedFieldsAt` already follows, and what keeps a thin client's row
   bytes from being offered a second layout to be mistaken for.
4. Pin it the way the thin path is pinned: the id read out of each recorded
   response must be the id the client's next frame drives. That cross-check is
   what makes the field the right one rather than a consistently decoded one, so
   the recording has to keep its client frames — extend the hex fixture, or
   record the drives alongside.
5. Then `TestOCIRefCursorBindOutputYieldsNoID` inverts, and the OCI paragraph in
   `docs/oracle.md` ("Learning a REF cursor's id") and the thick-client sentence
   in `docs/approvals.md` come out.

## Files

- `internal/proxy/oracle/refcursor_bind.go` — the walk
- `internal/proxy/oracle/describe.go` — `dcursor`, `parseColumnDescribe`
- `internal/proxy/oracle/testdata/oci_refcursor_bind_output.hex` — the evidence
- `internal/proxy/oracle/capture_refcursor_test.go` — how it was recorded
- `docs/oracle.md`, `docs/approvals.md`
