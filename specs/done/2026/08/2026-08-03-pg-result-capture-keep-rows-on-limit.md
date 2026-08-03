# PostgreSQL result capture stores zero rows when a large query hits the row limit

## Goal

Make the PostgreSQL `DataRow` capture path **keep** the rows it has already
captured when a storage limit is reached, instead of throwing them away, and
surface the truncation to the API/UI so "0 rows stored" is never ambiguous.

## Why

Observed on a real deployment: a single-column `SELECT … ORDER BY` returning
~3.3M rows (248 s, ~154 MB on the wire) logged **zero** result rows. The server
log for that connection:

```
{"level":"WARN","msg":"result capture refused - limits exceeded",
 "rows_captured":100000,"bytes_captured":1313955,
 "max_rows":100000,"max_bytes":104857600}
```

100 000 rows (1.3 MB — three orders of magnitude below the byte limit) had been
captured and were then discarded by
[`internal/proxy/postgresql/session.go:553`](internal/proxy/postgresql/session.go:553):

```go
query.truncated = true
query.capturedRows = nil // Discard all previously captured rows
```

Two problems:

1. **All-or-nothing on the PG row path only.** Every other capture path keeps
   the prefix and just stops: MySQL
   ([`internal/proxy/mysql/result.go:41`](internal/proxy/mysql/result.go:41)),
   MongoDB ([`internal/proxy/mongodb/result.go:212`](internal/proxy/mongodb/result.go:212)),
   Oracle ([`internal/proxy/oracle/intercept.go:319`](internal/proxy/oracle/intercept.go:319)),
   and even PostgreSQL's own COPY path
   ([`internal/proxy/postgresql/intercept.go:636`](internal/proxy/postgresql/intercept.go:636))
   all `break` and persist what they have. The PG `DataRow` path is the odd one
   out, so the exact case where a sample is most useful — a huge query — is the
   one that yields nothing.
2. **Truncation is invisible.** `truncated` is a session-local field: there is
   no column on the `queries` table
   ([`internal/store/models.go:323`](internal/store/models.go:323)), nothing in
   `openapi.yml`, nothing in the UI. "Stored 0 rows because we refused the
   capture" is indistinguishable from "the query genuinely returned nothing".

Note this fires at defaults: with no `DBB_QUERY_STORAGE_*` override,
`DefaultMaxResultRows = 100000` / `DefaultMaxResultBytes = 100 MB`
([`internal/config/config.go:380`](internal/config/config.go:380)) apply, and
any result set past 100k rows stores nothing at all.

No GitHub issue filed yet — one should be opened.

## Implementation

### 1. Keep the prefix (`internal/proxy/postgresql/session.go:547-565`)

Drop the `query.capturedRows = nil` line and align the branch with the other
protocols: set `query.truncated = true`, log at WARN with the same fields, and
stop appending. The `!query.truncated` guard on line 547 already prevents any
further capture, so no other change is needed in the loop.

Downgrade the message from "result capture refused" to something accurate, e.g.
`"result capture truncated - limits exceeded"`, matching the COPY-path wording.

### 2. Persist and expose the truncation flag

- Migration `internal/migrations/sql/YYYYMMDDHHMMSS_queries_results_truncated.{up,down}.sql`
  adding `results_truncated boolean not null default false` to `queries`.
- `store.Query` ([`internal/store/models.go:323`](internal/store/models.go:323)):
  new `ResultsTruncated bool` field, `bun:"results_truncated"`.
- Populate it in `logQuery`
  ([`internal/proxy/postgresql/intercept.go:332`](internal/proxy/postgresql/intercept.go:332))
  from `s.currentQuery.truncated` (and `s.copyState.truncated` on the COPY
  branch), then do the same for the three other protocols, which already track
  a local `truncated`/`break` and currently drop it on the floor.
- Add `results_truncated` to the query schema in
  [`internal/api/openapi.yml`](internal/api/openapi.yml) and render a badge on
  the query detail page in `front/` — "results truncated at N rows" is the whole
  point of the change.

### 3. Tests

- `internal/proxy/postgresql/intercept_test.go:1637` currently asserts the
  discard semantics — flip it to assert the prefix is retained and
  `truncated == true`.
- Add an integration case in
  [`internal/proxy/postgresql/integration_test.go:292`](internal/proxy/postgresql/integration_test.go:292)
  (it already sets `MaxResultRows: 1000`): run a `generate_series` SELECT past
  the limit, assert exactly `MaxResultRows` rows are stored and the query row
  reports `results_truncated = true`.

## Files

- `internal/proxy/postgresql/session.go:547` — the discard, the core fix.
- `internal/proxy/postgresql/intercept.go:332` — `logQuery`, flag plumbing.
- `internal/proxy/{mysql/result.go,mongodb/result.go,oracle/intercept.go}` —
  already keep the prefix; only need the flag plumbed through.
- `internal/store/models.go:323` + `internal/migrations/sql/` — persistence.
- `internal/api/openapi.yml`, `front/` — surfacing.
