---
model: opus
effort: medium
---

# An object or XMLTYPE column captures its locator, with its own value sitting five bytes further along the row

## Goal

Capture an opaque (`SYS.XMLTYPE`) or named-object column as the object's own
image — the attribute values the row already carries — instead of the 36-byte
locator in front of it.

## Why

Found on 2026-09-22 while implementing
`2026-09-22-03-oracle-row-capture-drops-every-row-of-a-fetch-carrying-a-lob.md`.

A LOB column captures as a marker because the data genuinely is not on the wire:
the locator names a LOB the client fetches separately, and dbbat must not issue
reads of its own to chase it. An **object** column is not in that position. Its
image travels in the same row, a few bytes behind the locator, and the row walk
already steps over it — `skipObjectImage` reads the image's length header to
find where the next column begins, and then throws the image away.

So `query_rows` currently holds, for a `XMLTYPE('<a/>')` column,
`000000240022020800000000000000000000000000020100000000000000000000000000` —
a handle, per-fetch, meaning nothing to a reader — while `3c 61 2f 3e` (`<a/>`)
is eighteen bytes further along the same packet and already located. The same
holds for `dbbat_cap_obj(1, 'x')`, whose image is `84 01 08 02 c1 02 01 78`:
a header, then the NUMBER `1` and the string `x` in the ordinary row encoding.

Two fixtures pin the locator as the captured value today
(`TestOCIRowCaptureCarriesTheDescribesColumnNames`,
`TestOCI64RowCaptureCarriesTheDescribesValues`), which is why it was left alone
rather than changed in passing: it is a deliberate value in two recent specs,
and swapping it is a decision of its own rather than a detail of the LOB fix.

## Implementation

1. `skipObjectImage` (`ttc_decode.go`) already returns the image's bounds in all
   but name — have it return the image bytes as well as the resume offset.
2. Decode the image. Its first bytes are a small header (`84 01 08` / `85 01 0c`
   in the two recordings) followed by what looks like the attributes in the same
   length-prefixed encoding the row uses. Measure it against the three object
   columns of `testdata/oci64_describe.hex` frame 2 (`dbbat_cap_obj(1,'x')`,
   `dbbat_o(2)`, `dbbat_cap_object_with_a_long_name(3)`) and the XMLTYPE of
   `testdata/oci64_lob.hex` — four samples, three of them with known attribute
   values, is enough to tell a header from a value.
3. Fail closed: an image that does not decode keeps today's locator hex rather
   than producing a plausible-looking object.
4. Update the two pinned fixtures' expectations, and `ociLOBXMLLocator` in
   `lob_rowcapture_test.go`, in the same commit as the decode.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `skipObjectImage`, `readRowColumn`
- `internal/proxy/oracle/lob_rowcapture_test.go` — `ociLOBXMLLocator`
- `internal/proxy/oracle/describe_wide_test.go`,
  `internal/proxy/oracle/describe_wide64_test.go` — the two pinned `OBJ` values
- `docs/oracle.md` — "LOB, opaque and object columns are locators, not values"
