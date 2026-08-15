---
model: sonnet
effort: high
---

# Demo-mode seeded sessions read as a broken query chain on /app/audit

## Problem

On https://demo.dbbat.com/app/audit the "Query history" card shows **Broken**
with, for the seeded session (1 session, 3 statements):

> chain_seq 0 … the session closed at 2026-08-10T00:04:00Z with 3 statements
> in its history but carries no head stamp: closing a session that logged
> statements always seals its head, so the stamp was removed — and with it the
> only thing that would have shown statements deleted from the end of the
> session

Nobody tampered with anything. The verifier is doing exactly what it is
designed to do — a *closed* session that has chained statements but no
`query_chain_mac` is indistinguishable from a stamp deletion, so it is a break
([chain_verify.go:691](internal/store/chain_verify.go:691)). The bug is that
the demo seeder manufactures precisely that state:

- `seedDemoHistory` ([main.go:1366](main.go:1366)) creates the connection via
  `store.CreateConnection` and the statements via `store.CreateQuery`, so the
  per-connection query chain itself is built correctly (that is why the break
  is reported at the head stamp, not inside the chain).
- It then "closes" the session with a **raw SQL UPDATE** that back-dates
  `connected_at` / `last_activity_at` and sets `disconnected_at` directly
  ([main.go:1409-1418](main.go:1409)). That bypasses `store.CloseConnection`,
  which is the writer that seals the chain head into `query_chain_mac` (keyed
  stamp + `query_chain_stamp_version`, see
  [connections.go](internal/store/connections.go) and `docs/audit-chain.md`).

So every demo restart re-seeds a session that is closed, has statements, and
carries no head stamp → the audit page permanently shows "Chain broken" on a
public demo, which is the worst possible advertisement for the
tamper-evidence feature.

Side effect of the same bypass: the seeded session gets a `connection.opened`
audit entry (written by `CreateConnection`) but never a `connection.closed`
one, since only `CloseConnection`/the reconcile write those.

## Proposal

Make the demo seeder close its sessions through a path that seals the chain,
then re-date the row — connection columns are deliberately unsealed, so
rewriting timestamps after sealing does not disturb the stamp:

1. In `seedDemoHistory`, after inserting the statements, close the session via
   `store.CloseConnection` (or, if its signature doesn't fit seeding, a small
   store helper that reuses the same seal-from-stored-statements routine the
   close path / reconcile use — do **not** hand-roll a second MAC
   implementation in `main.go`).
2. Keep the existing UPDATE for back-dating `connected_at`,
   `last_activity_at`, `disconnected_at`, `queries`, `bytes_transferred`, but
   run it *after* the seal and stop setting `disconnected_at` raw if the close
   path already wrote it — only override it to the staged historical instant.
3. Verify: `DBB_RUN_MODE=demo` boot, then `dbbat audit verify --queries` must
   exit 0 and `GET /api/v1/audit/verify/queries` must report no break; the
   /app/audit page shows the chain as intact.

## Resolved open questions

**Q:** The `connection.closed` audit entry written by the real close path will
carry the wall-clock close time, not the staged demo timestamp — accept that,
or pass the staged time through?

**Decision:** Pass the staged `disconnected_at` through to the evidence entry.
The seeding path must produce a `connection.closed` audit entry whose
timestamp matches the back-dated session, so the demo's audit trail is
self-consistent. Do not skip the entry — its absence is the other half of the
current inconsistency.

## Implementation Plan

1. **`internal/store/connections.go`** — extract the body of `CloseConnection`
   into an unexported `closeConnection(ctx, uid, closedAt time.Time) error`
   that takes the close instant as a parameter instead of calling
   `time.Now()` internally. `CloseConnection` becomes a one-line wrapper
   (`closeConnection(ctx, uid, time.Now())`). Add an exported
   `CloseConnectionAt(ctx, uid uuid.UUID, closedAt time.Time) error` — the same
   wrapper with the caller-supplied instant — for the demo seeder. Both share
   the exact chain-sealing SQL and `recordConnectionClosed` call; no second MAC
   implementation. `recordConnectionClosed`/`connectionClosedEvent` already
   read `DisconnectedAt` off the row `RETURNING` gave back, so passing a
   back-dated `closedAt` automatically makes the `connection.closed` audit
   entry carry that same staged instant — satisfies the resolved open
   question with no change to `connection_audit.go`.
2. **`main.go` (`seedDemoHistory`)** — after the `CreateQuery` loop, call
   `dataStore.CloseConnectionAt(ctx, conn.UID, disconnectedAt)` (the existing
   `disconnectedAt := lastActivityAt.Add(time.Minute)` value) instead of
   leaving the row open for the raw UPDATE to close. Then keep the raw UPDATE
   for back-dating, but drop its `disconnected_at` `Set(...)` — the close call
   already wrote the correct staged value onto the row and its audit entry;
   overwriting it again afterward would race the two writes and could leave
   the audit entry's timestamp (captured at close time) disagreeing with the
   row (rewritten after). Keep `connected_at`, `last_activity_at`, `queries`,
   `bytes_transferred` in the UPDATE — chain sealing does not depend on any of
   those columns, so rewriting them post-seal is safe, per the spec's premise.
   Check the `connection.opened` entry's `ConnectedAt`: `CreateConnection`
   stamps it from `time.Now()` before the seeder back-dates the row, so the
   open entry will carry wall-clock time while the row's `connected_at` is
   back-dated — note this as a pre-existing, separate inconsistency (open
   entry is not in this spec's stated scope, which is the chain break and the
   *close* entry) rather than silently "fixing" it by guessing at an
   unrequested API change to `CreateConnection`.
3. **Tests** (`internal/store`) — add a store-level test seeding a connection
   the same shape as `seedDemoHistory` (create → queries → `CloseConnectionAt`
   with a back-dated instant → back-date the row) and assert:
   - `VerifyQueryChain` reports no break (the core bug).
   - the `connection.closed` audit entry's `DisconnectedAt` equals the staged
     instant, not wall-clock.
4. **Verification** — `go build ./...`, targeted `go test ./internal/store -run
   ...`, then the full gate (`make build-binary`, `make lint`, `make test`).
   Optionally a throwaway `DBB_RUN_MODE=demo` boot on scratch ports to visually
   confirm `/app/audit`, per the spec — not required if the unit test covers
   the same assertion.
