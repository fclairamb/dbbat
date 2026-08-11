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
