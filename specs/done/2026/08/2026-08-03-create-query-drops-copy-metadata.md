# `CreateQuery` silently drops copy_direction / copy_format

## Goal

Persist the COPY metadata the PostgreSQL proxy already computes, so a logged
`COPY … TO STDOUT` records its direction and format instead of two NULLs.

## Why

[`store.CreateQuery`](internal/store/queries.go:39) builds the row it inserts
field by field:

```go
result := &Query{
    UID:          newUIDv7(),
    ConnectionID: query.ConnectionID,
    SQLText:      query.SQLText,
    ...
}
```

`CopyDirection` and `CopyFormat` are not in that list, so they are dropped on
the floor. The PostgreSQL session does populate them
([`intercept.go`, logQuery](internal/proxy/postgresql/intercept.go:348)) —
`query.CopyDirection = &s.copyState.direction` and the mapped
`copyFormatToString(...)` — and the columns exist on the `queries` table and in
the API schema, but nothing ever reaches them.

Found while adding COPY capture coverage for
`2026-08-03-result-row-persistence-strategy`: the new test
`TestCopyCapture_RowsArePersisted`
(`internal/proxy/postgresql/copy_persist_test.go`) asserted the direction came
back and it was nil. Pre-existing and unrelated to that spec, so it was left
alone rather than fixed in passing.

No GitHub issue filed yet — one should be opened.

## Implementation

- Add `CopyFormat: query.CopyFormat` and `CopyDirection: query.CopyDirection`
  to the literal in `CreateQuery` (`internal/store/queries.go`).
- Check the other write path: `UpdateQueryCompletion` does not touch them
  either, which is fine as long as the insert carries them — but the
  PostgreSQL COPY path now inserts the parent row *bare* and completes it
  afterwards (see `resolveQueryRecord`), and the bare copy must keep the COPY
  fields. It already does; add a test so it stays that way.
- Re-enable the assertion in `TestCopyCapture_RowsArePersisted`
  (`assert.NotNil(t, persisted.CopyDirection)`) and extend it to check
  `copy_format` too.
- Confirm `ListQueries`' explicit column list projects both columns; add them
  if not.
