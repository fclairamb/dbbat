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

  Until then the docs overstate the new detection: `docs/audit-chain.md` and
  `website/docs/features/audit-chain.md` describe the emptied-session break
  without saying that an attacker who can `DELETE FROM queries` can equally
  `UPDATE connections SET connected_at` to buy the retention excuse — and
  `website/docs/compliance.md` is the one an assessor reads closely. Note the
  qualifier there whether or not the chain itself lands, and note that it only
  bites on a deployment that has `DBB_QUERY_STORAGE_RETENTION` set: with
  retention off the excuse is unreachable
  (`internal/store/chain_verify.go`, `queryRetention <= 0`).
- **The access record itself is unsealed.** Who connected, as which user, from
  which IP, against which database, under which grant — the connection row is
  audit evidence in its own right, and only its *statements* are currently
  tamper-evident.

## Resolved open questions

> Decide whether `connections` deserves a chain of its own — and if so, how it
> survives the retention sweep (chain per day/instance, a sealed high-water
> mark, or an `audit_log` entry per session open/close instead).

**Decision (2026-08-11): do not chain `connections`. Write a chained
`audit_log` entry on session open and on session close instead.** `audit_log` is
already HMAC-chained and is never reaped by retention, so a
`DELETE FROM connections WHERE uid = …` leaves evidence behind in a table the
delete does not touch, and no new chain has to be reconciled against
`CleanupOldQueryRows`.

Concretely, and in place of the sketch below:

- **No migration, no new chain columns on `connections`, no new verify walk.**
  Do not add `chain_seq` / `prev_mac` / `mac` to that table, and do not add a
  `--connections` mode to `dbbat audit verify`.
- Emit an `audit_log` entry from `CreateConnection` and from every writer that
  closes a session (`CloseConnection`, `CloseOrphanedConnections` /
  `ReclaimDeadInstanceConnections`), carrying the row's **immutable identity** —
  connection `uid`, user, database, source IP, `connected_at`, the run/instance
  stamps — and on close the `disconnected_at` plus the session's
  `query_chain_mac` stamp, so the sealed record points at the query chain it
  owned. Mutable counters (`last_activity_at`, `queries`,
  `bytes_transferred`) stay out.
- The entries go through the existing `audit_log` chain writer unchanged; they
  inherit its tamper-evidence for free. They must not fail a connection: a
  failed audit write is logged, not fatal to the session.
- **Land the docs honesty fix regardless**, which is the half that is true
  whether or not anything else ships: `docs/audit-chain.md` ("What it proves,
  and what it does not"), `website/docs/features/audit-chain.md`'s detection
  table, `website/docs/compliance.md`, and the connection-row acceptance comment
  in `checkStampedHead`. Say plainly that a whole-session delete is now
  detectable *only* through those `audit_log` entries, that `connected_at` on
  the `connections` row itself is still an unsealed column, and that the
  retention excuse it buys only bites on a deployment with
  `DBB_QUERY_STORAGE_RETENTION` set.

## Implementation

Sketch, not a decision — superseded by the decision above, kept for context:

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
