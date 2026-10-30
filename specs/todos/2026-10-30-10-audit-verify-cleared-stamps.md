---
model: opus
effort: high
---

# Audit chain: clearing a stamp together with what it protects passes verification

## Problem

Found by the security scan of 2026-10-30 (finding M10).

Severity: Medium. Needs write access to dbbat's storage database but not the key: the threat model the chain exists for (`docs/audit-chain.md`).

`docs/audit-chain.md` says verification detects every statement of a session being deleted, and a head stamp being "cleared outright". Two combinations pass verification anyway:

- **Query chain.** Delete all of a session's `queries` rows and set its `query_chain_mac` to NULL and `query_chain_len` to 0. `chainedConnectionUIDs` (`internal/store/chain_verify.go:342`) only lists connections that have a stamp or a surviving chained statement, so the session is never walked. A scoped walk (`--connection`) lands in `checkMissingStamp` (`chain_verify.go:693`) with length 0 and no survivors, which is the "logged nothing" case and is not a break. The session's chained `connection.closed` audit entry still records the original MAC and length (`internal/store/connection_audit.go`), but nothing compares them.
- **Row chain.** Delete the tail (or all) of a capture's `query_rows` and set the query's `row_chain_mac` to NULL. `checkStampedRowHead` returns early on a NULL stamp (`chain_verify.go:1180`), and `capturedQueryUIDs` (`chain_verify.go:1011`) no longer lists a fully emptied capture. Unlike the query chain, no NULL-stamp rule applies to rows at all, and no copy of the row stamp exists anywhere else.

## Proposal

1. Query chain: during `dbbat audit verify --queries` (and the REST twin), cross-check each closed connection's stamp columns against its chained `connection.closed` audit entry. A stamp missing or different from the sealed one is a break. Enumerate connections from those audit entries as well, so a session whose stamp and statements were both cleared is still walked.
2. Row chain: seal the capture's final head where the attacker cannot clear it without breaking a chain. Options: carry `(row_chain_len, row_chain_mac)` in a chained record written at the flush barrier, or treat a NULL row stamp on a query whose session closed normally as a break. Document what a crash before the flush barrier leaves, which is the one legitimate unstamped case.
3. Update `docs/audit-chain.md` ("What it proves") to match.

## Tests

- `internal/store/chain_test.go`: a closed session with statements, then all statements deleted and the stamp cleared: `VerifyQueryChains` reports a break. This fails before the fix.
- A finished capture with its tail rows deleted and `row_chain_mac` cleared: `VerifyRowChains` reports a break. This fails before the fix.
- Positive controls: a session that logged nothing, an open session younger than one sweep, and a session emptied by retention (stamp kept) verify as today.
