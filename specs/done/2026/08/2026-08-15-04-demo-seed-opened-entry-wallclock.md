---
model: sonnet
effort: medium
---

# Demo-seeded sessions' connection.opened evidence carries wall-clock connected_at

## Problem

Follow-up to `specs/done/2026/08/2026-08-15-03-demo-seed-unsealed-query-chain.md`,
which fixed the *close* half of the seeded sessions' audit story: the query
chain is now sealed via `Store.CloseConnectionAt` and the `connection.closed`
evidence entry carries the staged historical instant.

The *open* half still doesn't. `seedDemoHistory` (main.go) creates each demo
session via `store.CreateConnection`, which stamps `connected_at` from
`time.Now()` and writes the `connection.opened` audit entry from that row —
so the evidence entry records boot-time wall-clock. The seeder then back-dates
the row's `connected_at` with a raw UPDATE (to `epoch - openedAgo`, days in
the past).

Result: on every demo boot, each seeded session's `connection.opened`
evidence entry disagrees with its connection row on `connected_at` (by up to
~8 days). Per `docs/audit-chain.md` / `internal/store/connection_audit.go`,
sessions are "evidenced, not chained" and the evidence is useful precisely
**by comparison** with the row — a mismatch on an immutable identity field is
what a tamper investigation would flag. The public demo should not
manufacture one, for the same reason the chain-seal spec existed.

No verifier walk reports this today (nothing seals connection columns), so it
is cosmetic-but-visible: anyone comparing `GET /api/v1/audit?event_type=connection.opened`
against the connections list on demo.dbbat.com sees the discrepancy.

## Proposal

Mirror the shape of the close-side fix:

1. In `internal/store/connections.go`, let a seeding caller supply the open
   instant: extract `CreateConnection`'s body into an unexported helper taking
   `connectedAt time.Time` (wall-clock wrapper keeps the existing signature),
   and add an exported `CreateConnectionAt(ctx, userID, databaseID, sourceIP,
   connectedAt)` — same insert, same `connection.opened` evidence write, so
   the row and the entry agree by construction. No second evidence-writing
   path.
2. In `seedDemoHistory` (main.go), create each session with
   `CreateConnectionAt(..., openedAt)` and drop the `connected_at` set from
   the back-dating UPDATE (keep `last_activity_at`, `queries`,
   `bytes_transferred` — mutable counters, not part of the evidence identity).
3. Test (`internal/store`): create a connection via `CreateConnectionAt` with
   a back-dated instant and assert the row's `connected_at` and the
   `connection.opened` entry's `ConnectedAt` both equal the staged instant
   (reuse the harness/helpers from
   `TestQueryChainCloseConnectionAtSealsWithStagedTimestamp` in
   `internal/store/chain_test.go`).

## Resolved open questions

**Q:** Should `CreateConnection`'s public API change instead?

**Decision:** No — keep `CreateConnection(ctx, userID, databaseID, sourceIP)`
as-is for the five proxies and add `CreateConnectionAt` alongside, exactly as
`CloseConnectionAt` sits next to `CloseConnection`. Seeding is the only
caller that needs a supplied instant.
