---
model: opus
effort: medium
---

# A session whose statements are deleted *in full* is never walked, so its stamp is never checked

**No GitHub issue filed yet — one should be.**

## Goal

Make `Store.VerifyQueryChains` examine the stamp on a connection row whose
`queries` rows have all been deleted, instead of skipping the connection
entirely. Today a total wipe of a session's statements is invisible to
`dbbat audit verify --queries`, while deleting all but one of them is caught.

## Why

Found by the completeness audit of
`specs/done/2026/08/2026-08-10-06-seal-the-connection-query-chain-stamp.md`.
The sealing work made the per-connection stamp keyed and unforgeable, which
closed trailing deletion — but only for sessions the walker actually visits.

`chainedConnectionUIDs` (`internal/store/chain_verify.go`, ~line 311)
enumerates connections **from the `queries` table**:

```sql
SELECT DISTINCT connection_id FROM queries WHERE chain_seq IS NOT NULL
```

So a connection with zero surviving statements produces no row, is never
walked, and its `query_chain_mac` / `query_chain_len` /
`query_chain_stamp_version` are never compared against anything. An attacker
who deletes a session's statements *entirely* — rather than just its tail —
leaves a connection row carrying a stamp for statements that no longer exist,
and verification reports nothing.

The row-chain half of the same feature already solves this. `capturedQueryUIDs`
(`internal/store/chain_verify.go`, ~line 604) deliberately unions in
`row_chain_mac IS NOT NULL` for exactly this reason, and
`TestRowChainDetectsWipedCapture` pins it. The query-chain side simply never
got the same treatment — the asymmetry predates the sealing work and was out of
its scope.

This is the natural next hole once
`2026-08-10-22-drop-legacy-query-chain-stamp-acceptance.md` lands: with legacy
acceptance dropped, a stamped-but-empty session is the remaining way to remove
statements without tripping the chain.

## Implementation

- Mirror `capturedQueryUIDs`: have `chainedConnectionUIDs` also return
  connections with a non-null `query_chain_mac`, unioned with the ones found in
  `queries`, so a stamped connection is walked whether or not any statement
  survived.
- `checkStampedHead` then has to handle the zero-survivor case deliberately.
  A stamp claiming `query_chain_len > 0` over an empty chain is a **break** —
  that is the whole point. Mind the interaction with `TruncatedPrefix`
  (`internal/store/chain_verify.go`): retention deleting a session's statements
  down to nothing is a legitimate, non-malicious path once
  `DBB_QUERY_STORAGE_RETENTION` is set, and must not cry wolf. Distinguishing
  them is the substantive design work — a retention-emptied session and a
  wiped one look identical from the `queries` table alone, so the answer
  probably has to lean on the connection's own `disconnected_at` versus the
  retention horizon, or on retention clearing the stamp when it empties a
  session.
- Copy the test shape from `TestRowChainDetectsWipedCapture`
  (`internal/store/chain_test.go`), and add the retention counterpart so the
  no-cry-wolf half is pinned too.
- Legacy (`query_chain_stamp_version = 0`) rows need no special case at all
  now that `2026-08-10-22` has landed: such a row is already a break the moment
  it is walked, whatever survived. The only thing to check is the behaviour
  under `--allow-legacy-stamps`, where a wiped legacy session cannot be
  distinguished from a forged one without the key — leave that with the rest of
  the legacy caveat rather than turning it into a new break.

## Key files

- `internal/store/chain_verify.go` — `chainedConnectionUIDs`, `checkStampedHead`,
  `capturedQueryUIDs` (the reference implementation)
- `internal/store/chain_test.go` — `TestRowChainDetectsWipedCapture` is the shape
- `docs/audit-chain.md` — the "what it detects" table gains a row

## Decisions

### Enumerate from `connections`, not a UNION

`chainedConnectionUIDs` now selects from `connections` where the row carries a
stamp **or** an `EXISTS` finds a chained statement. That is the same union
`capturedQueryUIDs` makes, expressed without a `UNION`: `queries.connection_id`
is `ON DELETE CASCADE`, so a statement can never outlive its connection row and
the connections table is a complete index of both halves. A session that logged
nothing has no stamp and is still skipped — pinned by
`TestQueryChainSilentSessionIsNotWalked`.

### Retention vs. a wipe: the configured window, compared against `connected_at`

The spec's substantive question. A stamped session with zero surviving
statements is a **break**, unless `DBB_QUERY_STORAGE_RETENTION` can account for
it. The store is told its own retention window at construction
(`store.Options.QueryRetention`, wired from the same config the sweep reads, in
both `serve` and `dbbat audit verify`), and the test is:

> the sweep deletes by `executed_at < now - retention`, and every statement of a
> session ran at or after its `connected_at` — so a session that connected **at
> or after** the cutoff cannot have had a single statement reaped, and an empty
> one is a break.

With retention disabled, the default, the cutoff is the beginning of time and
nothing is excused. An excused session is reported as `TruncatedPrefix` (the
extreme of one) rather than skipped silently, so it still appears in the sweep's
counters.

Why this and not the alternatives:

- **Not "retention clears the stamp when it empties a session."** That makes a
  NULL stamp a legitimate, operator-visible state and hands an attacker a
  sanctioned erase path — one `UPDATE … SET query_chain_mac = NULL` and the
  session is excused. It also directly contradicts
  `specs/todos/2026-08-11-12-nulled-query-chain-stamp-is-not-a-break.md`, which
  wants a NULLed stamp to *become* a break.
- **Not "retention deletes the emptied connection row too."** It would keep
  verification config-free, and it matches what `CleanupOldQueryRows` already
  claims ("a closed connection never survives as an empty shell"), but it
  destroys access evidence — who connected, from where, under which grant —
  earlier than the retention policy nominally allows, and on a
  retention-enabled deployment the sweep would erase the evidence of a real wipe
  on its next tick.
- **Not a data-derived horizon** (e.g. the oldest statement left in the store).
  It is self-tuning and immune to config changes, but it collapses on a young or
  quiet store, where the oldest surviving statement is minutes old and therefore
  excuses nearly every session. It is also unsound in the other direction on a
  store whose history starts recently.
- **Not a persisted "highest cutoff ever swept" parameter.** It would remove the
  caveat below, but the value would be an unkeyed column an attacker can raise
  to excuse their own wipe — the same downgrade path that got version-0 stamps
  dropped in `2026-08-10-22`.

Accepted costs, both documented in `docs/audit-chain.md`:

1. The rule is the **sound** one, not the tight one: a session that connected
   before the cutoff but ran its statements after it is excused too. Closing
   that would need the deleted statements' timestamps.
2. **Raising or disabling `DBB_QUERY_STORAGE_RETENTION` moves the cutoff
   backwards**, so sessions the previous setting legitimately emptied can start
   reading as breaks. Lowering it never can.

### Open sessions are judged the same way

Previously `stampedPrefixMAC` excused *every* zero-survivor open session on the
grounds that retention empties an idle live session (the sweep never reaps an
open connection row). That blanket excuse is gone: an open session is judged by
the same `connected_at`-vs-cutoff rule, which strictly tightens it — a live
session emptied inside the retention window is now a break
(`TestQueryChainDetectsWipedOpenSession`).

### Legacy stamps keep their caveat

Per the spec's last bullet: under `--allow-legacy-stamps` an emptied version-0
session is counted as a legacy stamp, not turned into a new break. An unkeyed
stamp attests to nothing either way, so a wiped legacy session and a
retention-emptied one are the same bytes and no key separates them. Without the
flag it is already a break for the version-0 reason, unchanged.

### Not touched: the NULL-stamp hole

`conn.QueryChainMAC == nil` still returns early. That is
`specs/todos/2026-08-11-12`, deliberately left alone. Its own third
implementation bullet asked whether retention can leave a closed connection row
behind with its stamp intact — it can (the closed-but-recently-disconnected
pooled session pinned by `TestQueryChainRetentionEmptyingASessionIsNotABreak`),
which is what this spec's rule is for.
