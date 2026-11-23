# Oracle: python-oracledb thin describes do not parse, so row values are typed by guesswork

## Goal

`parseColumnDescribes` returns the column records of a python-oracledb thin
QueryResult (func 0x10) on Oracle 23ai, so `QueryResultV2.ColumnTypes` is filled
and row values are decoded by their type rather than by the type-less heuristic.

## Why

Replaying `testdata/python_thin_array_dml.pcapng`, both SELECTs come back with
the right column names but an empty `ColumnTypes`: the names come from
`scanAndPadColumnNames`, the fallback. Every value is then rendered by
`decodeOracleRawValue`, which guesses text, DATE, TIMESTAMP or NUMBER from the
bytes alone. It guesses well for the common types, but a negative NUMBER whose
bytes are printable ASCII is stored as text, BINARY_FLOAT/DOUBLE come out as
hex, and the LOB/LONG/object readings (`rowValueShapeOf`) are never offered,
because they key on the type code.

## Implementation

- Dump the describe of the two SELECTs in the recording and walk it against
  python-oracledb's `_process_describe_info` / `_process_column_info`
  (thin `messages/base.pyx`) to find where `parseColumnDescribesMode` stops.
- Likely suspects: a field the 23ai server adds for TTC field version 24
  (domain, annotations, vector dimensions) that the walk does not skip.
- Pin it with a replay test asserting `ColumnTypes` for both SELECTs of the
  recording, and re-run the whole corpus (`sql_extraction_survey_test.go`
  pattern) to check no other client's describes change.
