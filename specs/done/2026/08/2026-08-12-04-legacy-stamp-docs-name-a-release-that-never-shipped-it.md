# The legacy-stamp docs name a release that never shipped the stamp

## Goal

Correct the "0.23.x" framing around `query_chain_stamp_version 0` and
`--allow-legacy-stamps`, or confirm it is deliberate shorthand. Today several
places state that dbbat **0.23.x** wrote unkeyed connection stamps and that a
store "upgraded from 0.23.x" needs the escape hatch. Git says otherwise: the
whole audit chain is unreleased.

## Why

Checked while implementing
`specs/todos/2026-08-11-12-nulled-query-chain-stamp-is-not-a-break.md`, whose
implementation had to answer "did any released version close a connection
without stamping it?":

- `9ce1a5a feat(store): seal audit_log and per-connection query history with an
  HMAC chain` — 2026-08-10. `git merge-base --is-ancestor 9ce1a5a v0.23.2` is
  false, and `git tag --contains 9ce1a5a` is empty.
- `v0.23.2` is 2026-08-08 (`be2a947`), and `.release-please-manifest.json` still
  says `0.23.2`. Everything chain-related is in the ~410 commits since.

So **no released dbbat has a `connections.query_chain_mac` column at all**. The
only stores that can hold a version-0 stamp are ones running an intermediate
build off `main` (the dev/test images this project deploys by digest), not
0.23.x installations. A compliance-facing document that names a release as
having shipped a forgeable stamp is wrong in the direction that matters — it
describes an exposure that never existed in a released build, and points
operators at an upgrade path they were never on.

Places that say it:

- `internal/migrations/sql/20260810020000_connections_query_chain_stamp_version.up.sql`
  — "The stamp shipped in 0.23.x as a *verbatim copy* …"
- `docs/audit-chain.md` — "A store upgraded from 0.23.x has a version-0 stamp on
  every session it closed", "dbbat 0.23.x-era stores", "Sessions closed *before*
  0.24".
- `website/docs/features/audit-chain.md` — the `Sessions closed before 0.24 do
  not verify` warning box and its "**Upgrading from 0.23.x?**" paragraph.
- `internal/store/chain_verify.go` (`AllowLegacyStamps`, `checkLegacyStampedHead`)
  and `internal/store/models.go` (`QueryChainStampVersion`) — "written by 0.23.x".
- `main.go` — the `sessions_with_legacy_forgeable_head_stamp` comment
  ("Sessions closed before 0.24").

## Implementation

Decide first, then edit once:

1. If the intent is "pre-0.24 development builds", say that. Reword to "a store
   written by a pre-0.24 build" / "a pre-release 0.24 build", and in
   `website/docs/` drop the upgrade framing entirely — a released-version reader
   has no such rows, and the flag is then an internal migration aid rather than
   an upgrade instruction.
2. If 0.24 is genuinely meant to ship the chain and the version column
   together (which is what the code does), `--allow-legacy-stamps` and
   `sessions_with_legacy_forgeable_head_stamp` are dead on arrival for every
   real deployment. Consider whether the flag should ship at all, or ship
   undocumented on the website and documented only in `docs/audit-chain.md`.
   Note it is already slated for removal in 0.25.

Either way, keep `checkLegacyStampedHead` itself — a version-0 row is still a
break, and that is independent of who could have written one.

No GitHub issue yet — file one when picking this up.

## Resolved open questions

> Decide first, then edit once: (1) reword to "pre-0.24 development builds", or
> (2) 0.24 ships the chain and the version column together, so
> `--allow-legacy-stamps` and `sessions_with_legacy_forgeable_head_stamp` are
> dead on arrival — consider whether the flag should ship at all.

**Decision (2026-08-12): branch 2 — remove the flag now.** Take the reading that
0.24 ships the chain and the version column together, which is what the code
already does. Therefore:

- **Delete `--allow-legacy-stamps` outright**, rather than waiting for the 0.25
  removal already slated for it: the CLI flag in `main.go`, the
  `AllowLegacyStamps` option in `internal/store/chain_verify.go`, and the
  `sessions_with_legacy_forgeable_head_stamp` counter it feeds (including its
  `main.go` reporting). The REST endpoints never had an equivalent, so nothing
  changes there.
- **Keep `checkLegacyStampedHead` itself.** A `query_chain_stamp_version 0` row
  stays a break — that is independent of who could have written one, and it is
  the whole point of sealing the version inside the MAC. Only the escape hatch
  goes; the check does not.
- **Accept the consequence**: a dev/test store that already carries version-0
  rows becomes unverifiable with no escape hatch. That is fine — only
  intermediate `main` builds can hold such rows, and re-creating such a store is
  cheap.
- **Reword every "0.23.x" mention** in the same pass, to "a store written by a
  pre-0.24 development build" (or equivalent). Every site listed under `## Why`
  above, plus `CLAUDE.md`. Note that after the removal most of these passages
  shrink to a sentence rather than needing careful rewording — there is no flag
  left to explain, only the fact that a version-0 stamp does not verify.
- **Drop the upgrade framing entirely from `website/docs/features/audit-chain.md`**
  — no "Upgrading from 0.23.x?" paragraph and no "Sessions closed before 0.24 do
  not verify" warning box. A released-version reader has no such rows and needs
  no instruction.

## Key files

- `internal/migrations/sql/20260810020000_connections_query_chain_stamp_version.up.sql`
- `docs/audit-chain.md`, `website/docs/features/audit-chain.md`
- `internal/store/chain_verify.go`, `internal/store/models.go`, `main.go`
- `CLAUDE.md` — the `audit verify --allow-legacy-stamps` line under
  "CLI Commands" and the `--allow-legacy-stamps` /
  `sessions_with_legacy_forgeable_head_stamp` passage under "Security"
