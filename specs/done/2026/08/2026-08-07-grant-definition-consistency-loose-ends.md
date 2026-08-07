---
model: sonnet
effort: medium
---

# Loose ends from grants-reference-definitions

## Goal

Close four small consistency gaps left by
[2026-08-06-04-grants-reference-definitions-only.md](../done/2026/08/2026-08-06-04-grants-reference-definitions-only.md).
None is an authorization hole — the completeness audit of that spec confirmed the
authorization paths are correct — but each is a place where two parts of the system
disagree about the same fact, which is how holes start.

## Why

Found by the independent audit of the definitions rework, recorded here rather than
widening that spec's already 70-file diff.

## Implementation

**1. `ListGrants(ActiveOnly: true)` ignores the definition's `is_active`.**
`internal/store/grants.go` (~:288-305) filters on the grant's own window and revocation but
not on whether its definition is still active, while `GetActiveGrant` (~:186) does. After an
operator deactivates a definition, the proxy correctly refuses the connection, but the UI and
the non-admin database-visibility checks (`internal/api/servers.go` ~:494-502,
`internal/api/keys.go` ~:207) still present the database as accessible. Add the same
definition-active filter to `ListGrants` when `ActiveOnly` is set, and cover it with a store
test that deactivates a definition and asserts the grant drops out of both paths.

**2. The grants list renders shape fail-open.**
`front/src/routes/_authenticated/grants/index.tsx` (~:186) does `g.definition?.controls ?? []`
— an absent definition would render as "no controls", i.e. unrestricted, which is the opposite
of the backend accessor's fail-closed convention (`internal/store/models.go` ~:537-543, which
returns every control when the definition is missing). Unreachable today given the `NOT NULL`
FK and the unfiltered attach, but the display convention should match the enforcement
convention. Render an explicit "unknown" state rather than an empty control list.

**3. An archived definition version is readable by uid.**
`handleGetGrantDefinition` (`internal/api/grant_definitions.go` ~:417-458) gates non-admins on
the fetched row's `is_active` and `AppliesToGroups` but not on `archived_at`, while listings
filter `archived_at IS NULL`. The exposure is narrow — uid-only, still scope-gated, and a
caller holding a grant from that version already sees the same content embedded in
`GET /grants` — so this is consistency, not a fix. Add the `archived_at IS NULL` check for
non-admins, keeping archived versions readable to admins and to a caller resolving their own
grant's pinned version.

**4. Backfill pass 2 matches shape without scope.**
The migration's second pass (`internal/migrations/sql/20260806020000_grants_reference_definitions.up.sql`
~:83-121) pins a legacy grant to any active definition with an identical shape, without
checking that the definition's `database_uids` covers the grant's own database. Behaviourally
inert — auth never consults the definition's database scope, and the shape is identical by
construction — but such a grant could not be re-issued through the assign endpoint, which does
enforce scope. Decide whether to tighten the pass (add the scope predicate, falling through to
pass 3's synthesized definition when it fails) or to document the asymmetry; a corrective
migration is only worth it if a real deployment is affected, so check before writing one.

No GitHub issue yet — one should be filed.
