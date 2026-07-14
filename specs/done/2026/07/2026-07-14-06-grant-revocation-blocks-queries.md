---
model: opus
effort: high
---

# Revoking a grant must block queries on live connections and disconnect them

Originating issue: [#252](https://github.com/fclairamb/dbbat/issues/252)

## Problem
When an admin revokes a grant, connections that were already established under
that grant keep working — the revocation is only consulted at connect/auth time,
not on subsequent queries. A revoked user can continue running queries on an
open session indefinitely.

## Proposal
- Re-check grant validity on every query, not just at connection time. The
  per-command grant checks already exist for quotas/expiry (PostgreSQL
  `internal/proxy/postgresql/auth.go`, Oracle
  `internal/proxy/oracle/intercept.go`, MySQL `internal/proxy/mysql/auth.go`);
  extend them to treat a revoked grant as invalid and reject the query with a
  clear error.
- Proactively **disconnect** live connections whose grant was just revoked,
  rather than waiting for their next query. Needs a way to look up active
  sessions by grant id and signal them to close — likely a registry of active
  sessions (per proxy or in `internal/proxy/shared`) that the revoke API
  (`internal/api/grants.go`) can notify.
- Avoid a hot DB lookup per query: consider a shared invalidation signal /
  cache (the codebase already has `internal/cache` for auth shared by API +
  proxies) so revocation propagates promptly without polling the store on every
  command.

Concurrency and lifecycle correctness are the crux here — closing a session
from an external goroutine must interleave safely with the session's own
read/write loops across all three protocols.

## Implementation Plan

The existing mid-query `LimitGuard`/watchdog (`internal/proxy/shared/limits.go`)
already gives us, for every protocol, both a per-command check (`checkQuotas`)
and a proactive teardown path (`onLimitViolation` fired by `guard.Watch`). We
extend that same seam with a revocation signal instead of inventing a parallel
mechanism.

1. **Shared revocation registry** — new `internal/cache/revocation.go`:
   - `RevocationHandle` wraps an `atomic.Bool`; `Revoked()` and `Flag()` are
     nil-safe.
   - `RevocationRegistry` maps `grantUID -> set of *RevocationHandle`, guarded
     by a mutex. `Register(grantUID) -> *RevocationHandle`, `Deregister(...)`,
     and `Revoke(grantUID) -> int` (flips every live handle's flag, returns how
     many sessions were signalled). All nil-/`uuid.Nil`-safe.
   - Lives in `internal/cache` per the spec hint (already shared by API +
     proxies, no import cycle — cache imports only stdlib).

2. **Store owns one registry instance** — `store.New` creates
   `s.revocations = cache.NewRevocationRegistry()`; add `Revocations()` getter.
   The store is the object both the API server and every proxy session already
   hold, so no constructor plumbing is needed and there is a single shared
   instance process-wide.

3. **API revoke path** (`internal/api/grants.go`) — after a successful
   `store.RevokeGrant`, call `s.store.Revocations().Revoke(uid)` and log the
   count of live sessions signalled.

4. **LimitGuard revocation awareness** (`internal/proxy/shared/limits.go`):
   - Add `ErrGrantRevoked`.
   - Add a `revoked *atomic.Bool` field + fluent `WithRevocation(*atomic.Bool)`
     setter (keeps `NewLimitGuard`'s signature stable — no test churn).
   - `Check()` returns `ErrGrantRevoked` first (most authoritative).
   - `Watch()`'s "nothing to enforce" short-circuit also keeps running when a
     revoked flag is present. Result: within one poll interval (≤250ms) of a
     revoke, `Check()` trips → `onLimitViolation` force-closes both conns =
     proactive disconnect, reusing the tested teardown.

5. **Per-protocol wiring** (PostgreSQL, Oracle, MySQL):
   - Add a `revocation *cache.RevocationHandle` field to each session.
   - Where the guard is built (PG `proxyMessages`, Oracle `proxyMessages`,
     MySQL `OnAuth`): `s.revocation = store.Revocations().Register(grant.UID)`
     then `guard = NewLimitGuard(...).WithRevocation(s.revocation.Flag())`.
   - Deregister on session teardown (PG/Oracle `cleanup`, MySQL a dedicated
     deferred `deregisterRevocation` in `Run`).
   - Add a revoked check to each `checkQuotas` (PG/Oracle) / `runIntercepted`
     (MySQL) so the next command on a live connection is rejected with a clean
     protocol error even before the watchdog fires.

6. **Tests**: registry unit tests (`internal/cache`); guard revocation
   Check/Watch tests (`internal/proxy/shared`); per-protocol tests asserting a
   revoked grant makes `checkQuotas`/`runIntercepted` reject and the watchdog
   force-closes the conns. Run with `-race` for the concurrent teardown.

Race note: the tiny window between `GetActiveGrant` (which already filters
`revoked_at IS NULL`) and `Register` is accepted — a revoke landing in that
microsecond window predates a live session and is caught by the next command's
DB-truth on reconnect; per the spec we deliberately avoid a hot DB lookup per
query.
