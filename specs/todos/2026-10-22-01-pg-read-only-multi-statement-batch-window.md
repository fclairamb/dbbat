# PostgreSQL read_only: close the multi-statement window behind the pin tripwire

## Goal

A read-only session must not be able to run a write in the same batch as the
statement that turned `default_transaction_read_only` off, even through a route
the text scans cannot see.

## Why

The read-only pin is protected twice (see "Read-only grants" in
`docs/postgresql.md`): text scans refuse every known spelling of a pin escape,
and a tripwire ends the session when PostgreSQL ≥ 14 reports
`default_transaction_read_only` changed. The report is sent once per batch,
just before `ReadyForQuery`, so this still writes:

```sql
SELECT f(); COMMIT; INSERT INTO t VALUES (1)
```

where `f` is an existing function that calls `set_config` with a name built at
run time. The write-keyword check only reads the first keyword of the string,
and the INSERT runs in a fresh implicit transaction that the pin no longer
covers. The same holds for an extended-protocol pipeline (`Execute` × 3, then
`Sync`). It needs such a function in the target database, so it is narrow, but
it is the one route left.

## Implementation

Two options, not exclusive:

- Under `read_only`, run the write classifier on **every** statement of a
  simple-query string rather than on its prefix: split on `;` outside literals,
  dollar quotes and comments (a scanner in `internal/proxy/shared` next to
  `readonly_scan.go`; dollar quoting would be new there), and refuse if any part
  writes. That alone does not close it — the write keyword check is defense in
  depth — so also:
- Under `read_only`, refuse a simple-query string containing `COMMIT`, `END` or
  `ROLLBACK` followed by another statement, and in the extended protocol refuse
  a `Parse` of `COMMIT`/`END` when the pipeline holds further `Execute`s before
  its `Sync`. Without a transaction boundary inside the batch, a changed
  *default* cannot reach any statement of that batch, and the tripwire handles
  the next one.

Key files: `internal/proxy/postgresql/intercept.go` (`validateStatement`,
`handleParse`), `internal/proxy/postgresql/session.go` (`receiveFromUpstream`).
Integration coverage goes next to `TestIntegration_ReadOnlyGrant_TerminatesOnLostPin`.
