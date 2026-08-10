---
model: opus
effort: medium
---

# Drop acceptance of the legacy (version 0) query-chain head stamp

## Goal

Stop `checkStampedHead` from accepting `query_chain_stamp_version = 0` —
the pre-0.24 verbatim copy of the head MAC — and pick the release it happens in.
After that, the only stamp that verifies a session's tail is the keyed one, and
the standing downgrade path is gone.

## Why

`2026-08-10-06-seal-the-connection-query-chain-stamp.md` made the connection's
head stamp a keyed MAC, sealed the format version inside it, and left version 0
accepted for one reason only: the chain key never enters the database, so
nothing can re-seal what 0.23.x wrote, and failing every pre-upgrade session
would report a break on every closed session in an upgraded store.

That acceptance is the last unkeyed path in the query chain, and it is
*deliberately* a door:

- an attacker with write access can delete the tail of a **sealed** session and
  replace the whole stamp — raw head MAC, matching length, version back to `0` —
  and verification will accept it. What it cannot do is look like a keyed
  verification: the session lands in `legacy_stamps`
  (`sessions_with_legacy_forgeable_head_stamp` in the CLI). Visible, but not a
  break;
- until then the docs have to keep qualifying the trailing-deletion guarantee
  (`docs/audit-chain.md`, `website/docs/features/audit-chain.md`,
  `website/docs/compliance.md`), which is exactly the sort of asterisk an
  assessor reads closely.

The count is self-liquidating: every session closed by 0.24+ is sealed, and
pre-upgrade sessions age out with `DBB_QUERY_STORAGE_RETENTION`. A deployment
with no retention keeps them forever, which is why this is a decision to make
rather than something to leave to time.

No GitHub issue yet — file one when picking this up.

## Implementation

- Decide and document the release. The honest framing for release notes: after
  version X, sessions closed by 0.23.x report a break, and the remedy is to have
  already recorded that they were legacy (the count was reported for every
  release in between).
- `internal/store/chain_verify.go`, `checkStampedHead`: remove the version 0
  fallback, so a legacy stamp becomes a break with its own reason
  ("stamped by a version of dbbat whose stamp was not keyed; its tail cannot be
  verified") rather than being silently reclassified as a forgery.
- Keep `LegacyStamps` on the result, now counting rows that *broke* for this
  reason, or drop it — decide, do not leave both meanings live.
- `internal/store/chain_test.go`: `TestQueryChainLegacyStampStillVerifies` and
  `TestQueryChainDowngradeToRawStampIsReportedAsLegacy` both invert. Keep them,
  renamed, asserting the new outcome —
  `TestQueryChainDetectsStampVersionDowngrade` should be unaffected, and if it
  is, that is worth noticing.
- Consider offering the operator an escape hatch before the flag day rather
  than after: a `dbbat audit verify --queries --allow-legacy-stamps` that keeps
  today's behaviour for one release, so an upgrade does not turn a monitoring
  job red overnight.
- Update the three docs plus the root `CLAUDE.md` bullet, all of which currently
  describe the two-format world.
