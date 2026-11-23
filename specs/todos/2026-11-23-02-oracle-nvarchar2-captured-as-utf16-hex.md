# Oracle: NVARCHAR2 / NCHAR values are captured as UTF-16 hex

## Goal

A national-character column (NVARCHAR2, NCHAR, NCLOB inline) is captured as
text in `query_rows`, like VARCHAR2.

## Why

Found while recording `testdata/python_thin_array_dml.pcapng`: an NVARCHAR2
column holding `Bâtiment 1` was captured as `004200e200740069006d...`. The
server sends national-character data in the national character set, AL16UTF16
(UTF-16 big-endian) by default, and nothing in the row decoders converts it.
The same capture also showed a phantom row at the packet boundary, which the
ROW_HEADER walk (`compressedContinuationRowStart`) has since fixed; the UTF-16
rendering is what is left.

## Implementation

- The describe's column record carries the charset form (`csfrm`, 2 for
  national) and charset id; once describes parse for the client (see
  2026-11-23-01), carry `csfrm` into `columnDef` and decode `csfrm == 2` values
  as UTF-16BE (`unicode/utf16`) in `decodeRowValue`.
- Without a parsed describe, do not guess: UTF-16 of ASCII text is valid UTF-8
  with NULs in it, which `isPrintableUTF8Text` already refuses, so it stays hex.
- Record a fixture with an NVARCHAR2 column (python-oracledb and go-ora) and
  pin the rendered value.
