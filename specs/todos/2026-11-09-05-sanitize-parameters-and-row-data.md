---
model: opus
effort: medium
pr: batch
automerge: false
---

# Captured `parameters` / `row_data` are not NUL-sanitised, so a NUL drops the statement from history

## Problem

Found by the security scan of 2026-11-09 (finding L1). Severity: Low.
Triggerable by any authenticated dbbat user whose statement carries a captured
parameter value (or result row value) containing a NUL byte (U+0000) — realistic
on SQL Server, where `nvarchar` values may legitimately hold U+0000.

The store sanitises only the statement text: `internal/store/queries.go:41-55`
repairs `sql_text` (and `error`) to valid UTF-8 with NUL → U+FFFD. The captured
**parameters** (`QueryParameters.Values`) and **result rows** (`row_data`) are
marshalled to the `jsonb` columns without that repair (`queries.go:203-222`;
`internal/store/models.go`; schema `parameters jsonb`, `row_data JSONB`).
PostgreSQL rejects `\u0000` inside a `jsonb` value, so the `CreateQuery` insert
(and the chained MAC append that rides it) fails, and the statement — already
executed upstream — never lands in `queries` or the audit chain. The result is a
per-statement evidence gap that a user can trigger with an ordinary value, and
one the tamper-evident chain is meant to make impossible.

## Proposal

1. Apply the same repair used for `sql_text` (`ToValidUTF8` + NUL → U+FFFD, or
   the existing helper in `internal/store/queries.go:41-55`) to every string in
   `QueryParameters.Values` before it is marshalled to `parameters`, and to the
   string fields of captured `row_data` before it is marshalled.
2. Keep binary/`bytea`-style values (already base64-encoded) untouched — only
   decoded string values need the repair.
3. Make the repair a single shared helper so the three sites (sql_text, params,
   rows) cannot drift.

## Tests

- `internal/store/queries_text_test.go` (or a sibling): a `Query` whose
  `Parameters.Values` contains `"ab\x00cd"` inserts successfully and reads back
  with the NUL replaced (fails before the fix — the insert errors on invalid
  `jsonb`).
- A captured result row containing a NUL in a string column inserts and reads
  back repaired.
- Positive control: a parameter/row with ordinary UTF-8 (including multi-byte
  characters) is stored byte-identical (no over-escaping).

## To verify

- Whether `row_data` strings flow through a single marshal site or several
  (row-capture path in `internal/proxy/postgresql` and the MSSQL capture path),
  so the helper is applied everywhere a captured string is persisted.
