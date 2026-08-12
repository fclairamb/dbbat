# Deleting a whole session — connection row and all — is undetectable

**No GitHub issue filed yet — one should be.**

## Goal

Decide whether `connections` deserves a chain of its own, so that removing an
entire session (the connection row, which cascades to its statements and their
captured rows) leaves evidence. Today it leaves none.

## Why

Found while implementing
`specs/todos/2026-08-10-23-verify-stamps-of-fully-wiped-sessions.md`, which
closed the "statements deleted, connection row left behind" hole. The mirror
case is now the cheapest attack on the query history:

```sql
DELETE FROM connections WHERE uid = '…';
```

`queries.connection_id` and `query_rows.query_id` are both `ON DELETE CASCADE`,
so one statement removes the session, everything it ran and everything it read.
Nothing references the row afterwards, so nothing notices — `checkStampedHead`
even documents the acceptance explicitly ("A connection row that is gone is not
a break: retention deletes whole connections, queries and all"). Every other
deletion path is now covered: middle, start, end, and (since 2026-08-10-23) the
whole of a session's statements.

Two smaller things ride on the same missing chain:

- **`connected_at` is unsealed.** The emptied-session check added by
  2026-08-10-23 excuses a session that connected before the
  `DBB_QUERY_STORAGE_RETENTION` cutoff, and that timestamp is a plain column
  anyone with write access can backdate. A chained connection row would make
  the excuse unforgeable.
- **The access record itself is unsealed.** Who connected, as which user, from
  which IP, against which database, under which grant — the connection row is
  audit evidence in its own right, and only its *statements* are currently
  tamper-evident.

## Implementation

Sketch, not a decision — the design work is the point of the todo:

- The obvious shape is a store-wide chain over `connections`, like `audit_log`:
  `chain_seq`, `prev_mac`, `mac` over the row's immutable identity (`uid`,
  `user_id`, `database_id`, `source_ip`, `connected_at`, plus the run/instance
  stamps).
- The hard part is retention. `CleanupOldQueryRows` deletes closed connections
  by the sweep's cutoff, and a single global chain over a table retention
  deletes from is exactly what `docs/audit-chain.md` says the query chain was
  split per connection to avoid. A global connection chain would report a
  truncated prefix after every sweep — tolerable if the sweep only ever removes
  the *oldest* rows, which it does by `disconnected_at`, but `disconnected_at`
  is not the chain order (`connected_at` is), so a long-lived session breaks
  that assumption. Weigh: chain per day/instance, a sealed high-water mark, or
  an `audit_log` entry per session open/close instead of chaining `connections`
  at all (that table is already chained and never reaped).
- Mutable columns (`last_activity_at`, `queries`, `bytes_transferred`,
  `disconnected_at`, the chain stamp columns) must stay outside the MAC, for the
  same reason the query chain excludes the outcome columns.
- Docs: `docs/audit-chain.md` ("What it proves, and what it does not"),
  `website/docs/features/audit-chain.md`'s detection table, and the
  connection-row acceptance comment in `checkStampedHead`.

## Key files

- `internal/store/chain.go`, `internal/store/chain_verify.go` — the three
  existing chains and their walks
- `internal/store/connections.go` — `CreateConnection`, `CloseConnection`
- `internal/store/queries.go` — `CleanupOldQueryRows`, the retention sweep
- `docs/audit-chain.md`
