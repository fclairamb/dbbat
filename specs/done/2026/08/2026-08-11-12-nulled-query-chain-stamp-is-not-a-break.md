# A closed session whose stamp was NULLed verifies clean

## Goal

Make `checkStampedHead` treat a **closed** connection with
`query_chain_mac IS NULL` as a break, so deleting a session's trailing
statements and then clearing its stamp is no longer a clean verification.

## Why

`internal/store/chain_verify.go`, in `checkStampedHead` (~line 550):

```go
// Never stamped: the session logged nothing, or it is open and no sweep has
// reached it yet. Nothing to compare against.
if conn.QueryChainMAC == nil {
    return nil
}
```

That early return is correct for the two cases it names, and it is the *only*
thing standing between an attacker and the whole point of the stamp. The stamp
exists to catch a deletion from the **end** of a session's history. An attacker
with write access does not have to defeat it — a single

```sql
UPDATE connections SET query_chain_mac = NULL, query_chain_len = 0 WHERE uid = …
```

removes the thing that would have caught them, and `dbbat audit verify
--queries` then walks the truncated chain, finds every surviving statement
self-consistent, sees no stamp to compare against, and reports the session as
verified. Clearing the stamp is *cheaper* than forging one, and 0.24 — which
made the stamp a keyed MAC and stopped accepting the unkeyed pre-0.24 format
(`specs/done/…/2026-08-10-06`, `specs/todos/2026-08-10-22`) — raised the cost of
every other path against this one without touching it.

This is **pre-existing**, not a regression from that work: the early return has
been there since the stamp was introduced.

**It is not what `specs/todos/2026-08-10-23-verify-stamps-of-fully-wiped-sessions.md`
covers, and the two must not be conflated.** That spec is about a session whose
*statements* were all deleted while its stamp survived, and its fix is to union
`query_chain_mac IS NOT NULL` into `chainedConnectionUIDs` so such a connection
is walked at all. This is the mirror image — the statements survive and the
*stamp* is gone — and the union does nothing for it, because the connection is
already enumerated from `queries`. Implementing 2026-08-10-23 leaves this hole
exactly as it is.

No GitHub issue yet — file one when picking this up.

## Implementation

- In `checkStampedHead`, split the NULL-stamp case by
  `conn.DisconnectedAt`:
  - **open** session, no stamp: unchanged. `RefreshOpenChainStamps` may simply
    not have reached it yet, and a session that logged nothing never gets one.
  - **closed** session with surviving chained statements and no stamp: a
    **break**. Every writer that closes a connection also stamps it —
    `CloseConnection`, `CloseOrphanedConnections` / `ReclaimDeadInstanceConnections`
    — so a closed session that logged statements and carries no stamp is a state
    no code path in this build produces.
- Mind the legitimate NULL-stamp closes before declaring the break, and pin each
  as a no-cry-wolf test:
  - a closed session that logged **nothing** (`result.Verified == 0` and no
    chained rows) is stamped NULL on purpose — see
    `TestQueryChainRefreshLeavesSilentSessionUnstamped`;
  - a session closed by a build **older than the stamp** — check whether any
    released version closed connections without writing `query_chain_mac`. If
    one did, this needs the same shape of decision `2026-08-10-22` made for
    version-0 stamps (break by default, with a documented, expiring escape
    hatch), not a silent pass;
  - retention that reaped a closed session's statements down to nothing —
    verify whether `CleanupOldQueryRows` can leave the connection row behind
    with its stamp intact; if it can, the break must key off the stamp being
    NULL rather than off the survivor count.
- Consider whether `query_chain_len > 0 AND query_chain_mac IS NULL` deserves
  its own reason: the length column surviving a cleared MAC is a strong signal
  the row was edited rather than never stamped.
- Docs: `docs/audit-chain.md` (the "what it detects" list and the connection
  stamp section, which currently says "a NULL stamp is never itself a break"),
  and `website/docs/features/audit-chain.md`'s detection table.

## Key files

- `internal/store/chain_verify.go` — `checkStampedHead`, the NULL early return
- `internal/store/connections.go` — `CloseConnection`,
  `CloseOrphanedConnections`, `RefreshOpenChainStamps` (the three stamp writers)
- `internal/store/chain_test.go` — `TestQueryChainStampsHeadOnClose` and
  `TestQueryChainRefreshLeavesSilentSessionUnstamped` are the shapes to copy
- `docs/audit-chain.md`, `website/docs/features/audit-chain.md`
