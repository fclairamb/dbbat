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

## Decisions

Three choices this spec delegated to the implementer, and why they went the way
they did.

### 1. The release is 0.24 — the same one that introduces the keyed stamp

The framing above assumes a released 0.24 that *accepted* version 0 while
reporting the count, followed by a later release that stops. That release never
happened: the latest tag is **v0.23.2** and `main` is ~340 unreleased commits
ahead of it, so `2026-08-10-06` (the keyed stamp, the version column, the
`legacy_stamps` counter) has not shipped anywhere. There is nothing to give
"one release of warning" *in*, and no operator has a monitoring job watching a
counter that has never existed in a release.

So the keyed stamp and the end of version-0 acceptance ship together, in 0.24
(`bump-minor-pre-major` is on, so a `feat!` on 0.x lands as 0.24.0). No released
build ever has the downgrade door open by default — which is strictly better
than shipping it open and closing it later.

**What the release notes must say**, since the warning window does not exist:

> **Breaking:** `dbbat audit verify --queries` now reports a **break** on every
> session closed by 0.23.x. Those sessions carry an unkeyed head stamp — a
> verbatim copy of the last statement's MAC — which cannot be re-sealed (the
> chain key never enters the database) and attests to nothing: anyone who can
> write to the store can write it. Their statements still verify; what cannot be
> verified is that nothing was removed from their *end*.
>
> A walk stops at its first break, so on an upgraded store one pre-0.24 session
> hides every real break behind it. Pass
> `dbbat audit verify --queries --allow-legacy-stamps` for the duration of the
> transition: it restores the previous outcome, counting those sessions under
> `sessions_with_legacy_forgeable_head_stamp` instead of breaking the walk. Drop
> the flag once the count reaches zero — pre-upgrade sessions age out with
> `DBB_QUERY_STORAGE_RETENTION`, and a deployment that sets no retention keeps
> them forever. **The flag is removed in 0.25.**
>
> `GET /api/v1/audit/verify/queries` no longer returns `legacy_stamps`, and has
> no equivalent option: over REST such a session is always a break.

### 2. `LegacyStamps` stays, with exactly one meaning — and leaves the REST surface

It keeps the meaning it already had, "accepted despite an unkeyed stamp", and
never acquires the second one the spec offered ("broke for this reason"). A
count of rows that *broke* would be useless anyway: a walk stops at its first
break, so such a counter could only ever be 0 or 1.

That meaning is only reachable under `--allow-legacy-stamps` (decision 3), which
is CLI-only — so over REST the counter could only ever report `0`, and a
permanently-zero field is worse than no field. `legacy_stamps` is therefore
**removed** from the `/audit/verify/queries` response, the OpenAPI schema, the
generated TypeScript client and the admin audit panel's facts. One meaning,
live in exactly one place: `QueryChainResult.LegacyStamp`,
`QueryChainsResult.LegacyStamps` and the CLI's
`sessions_with_legacy_forgeable_head_stamp`.

### 3. Yes to the escape hatch — `--allow-legacy-stamps`, CLI-only, gone in 0.25

The deciding evidence is not "a monitoring job turns red overnight" (no released
build reports this counter, so no such job exists). It is that **a walk stops at
its first break**: without an opt-out, a single session closed by 0.23.x masks
every genuine break behind it, and on a deployment that sets no
`DBB_QUERY_STORAGE_RETENTION` it does so forever. That converts a security
improvement into a loss of detection, which is the opposite of the point.

CLI-only because that is where the exit code a monitoring job reads lives, and
because the REST endpoint is served by the process under audit — a
"tolerate this" switch there is a switch an attacker who owns the process would
already control. The flag is refused without `--queries`, it never launders a
*keyed* stamp relabelled as legacy (the version is inside the MAC, so a
relabelled row is held to the raw-head rule it claims and fails it), and it is
removed in 0.25.

### Not done

No GitHub issue was filed (the spec asks for one when the task is picked up):
opening a public issue is a publishing action and was left to the maintainer.
