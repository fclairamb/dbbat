# Stamp the query chain head on crash-orphaned connections

## Goal

Make the startup reconcile that closes crash-orphaned connections stamp
`query_chain_mac` / `query_chain_len` from the database, so a session whose
process died is as tamper-evident at its tail as one that closed cleanly.

## Why

`2026-08-09-tamper-evident-audit-chain.md` shipped the per-connection query
chain. `Store.CloseConnection` stamps the final chain head onto the connection
row, which is what makes deleting the *last* statements of a session detectable
— a truncated chain still verifies against itself, so only the recorded head
catches it.

`Store.CloseOrphanedConnections` / `ReclaimDeadInstanceConnections`
(`internal/store/connections.go`) close rows left open by a crashed process,
and they stamp nothing: the in-memory chain state died with that process. Every
crash-orphaned session therefore has an unprotected tail. On a pod that gets
rescheduled regularly, that is not a rare edge.

No GitHub issue yet — file one when picking this up.

## Implementation

- The head is recoverable without the crashed process: it is the highest
  `chain_seq` on that connection and its `mac`. `readQueryChainHead`
  (`internal/store/queries.go`) already does exactly that lookup.
- The honest caveat: stamping at reconcile time seals whatever survived *then*,
  not what the session actually wrote. If someone deleted trailing statements
  between the crash and the reconcile, the stamp blesses the truncated chain.
  That is still strictly better than no stamp, but it must be said in
  `docs/audit-chain.md` — and it argues for stamping in the same statement that
  sets `disconnected_at`, not in a later pass.
- `closeOrphans` currently runs one scoped `UPDATE` over many rows. A correlated
  subquery can set both columns in that same statement:
  `query_chain_mac = (SELECT mac FROM queries q WHERE q.connection_id = c.uid
  ORDER BY chain_seq DESC LIMIT 1)`, and likewise for the length. Verify it does
  not regress the reconcile's cost on a large store, and keep it NULL for
  connections that logged nothing.
- Test in `internal/store/`: write statements on a connection, impersonate a
  dead run (`SetRunID` — the existing orphan tests do this), reconcile, then
  delete a trailing statement and assert `VerifyQueryChain` reports the break.
