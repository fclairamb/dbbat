# PostgreSQL: captured rows lose their column names when an Execute gets no RowDescription

## Goal

Captured result rows of an extended-protocol `Execute` carry the real column
names and type OIDs, even when the upstream sends no `RowDescription` right
before that statement's `DataRow`s.

## Why

Found while fixing the pipelined-batch row attribution
(`2026-10-30-04-pg-pipelined-batch-row-attribution`). The session reads column
metadata only from a `RowDescription` it sees while an `Execute` is queued
(`captureRowDescription` in `internal/proxy/postgresql/session.go`). Two common
flows never send one at that point:

- pgx (and other drivers that cache prepared statements) describe the
  *statement* once in a separate `Parse`/`Describe`/`Sync` exchange, then send
  only `Bind`/`Execute`. No `Execute` is queued when that `RowDescription`
  arrives, so it is dropped, and every later row is stored as `col_0`, `col_1`…
  and decoded with OID 0 (wrong for binary result formats).
- A portal paged with `Execute(maxRows)`: only the first page follows the
  `Describe` portal, so pages 2 and later have no metadata.

## Implementation

- Remember the `RowDescription` answering a `Describe` of a statement (keyed by
  the prepared statement name, in `extendedState`) or of a portal (keyed by the
  portal name). The `Describe` messages are already parsed in `handleDescribe`;
  queue which object each pending `Describe` targets so the upstream leg knows
  where an incoming `RowDescription` belongs.
- In `handleExecute`, seed the new `pendingQuery`'s `columnNames`/`columnOIDs`
  from the portal's cached description, falling back to its statement's.
- Record the result format codes from `Bind` so binary columns are decoded
  with their OID rather than read as text.
- Tests: loopback unit tests (statement described in an earlier batch, then
  executed in a later one; a portal paged three times), and an integration test
  with a plain pgx `QueryRow` whose captured row must carry the column name.
