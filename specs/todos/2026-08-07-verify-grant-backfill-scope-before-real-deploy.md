# Verify the grants-reference-definitions backfill against real data before it ships

## Goal

Before migration `20260806020000_grants_reference_definitions` runs against any
environment holding real (non-test) grant data — in particular the actual production
dbbat deployment — confirm its backfill pass 2 didn't pin a grant to a definition
whose database scope excludes the grant's own database.

## Why

Found while implementing
[2026-08-07-grant-definition-consistency-loose-ends.md](2026-08-07-grant-definition-consistency-loose-ends.md)
item 4. Backfill pass 2 (`internal/migrations/sql/20260806020000_grants_reference_definitions.up.sql`
~:87-119) matches a legacy grant to an active definition by shape (controls, quotas,
approval patterns, approver groups) without checking that the definition's
`database_uids` covers the grant's own database. That's behaviourally inert — auth
never consults a definition's database scope, only the assign endpoint does, at
issuance time — but a grant backfilled this way could not be re-issued through the
assign endpoint today, which is a genuine (if narrow) inconsistency.

No corrective migration was written because, as of 2026-08-07, this migration has not
shipped in any tagged release (latest tag at the time is `v0.22.0`; the migration
landed after it) and the check below returned zero rows against the shared dev
database. The asymmetry is documented in the migration file itself rather than fixed,
per the loose-ends spec's instruction not to rewrite a migration without evidence a
real deployment is affected.

## Implementation

Before (or shortly after) this migration reaches the real Stonal dbbat deployment,
run this against that database:

```sql
SELECT ag.uid, ag.database_id, gd.slug, gd.database_uids
FROM access_grants ag
JOIN grant_definitions gd ON gd.uid = ag.grant_definition_id
WHERE gd.database_uids IS NOT NULL
  AND array_length(gd.database_uids, 1) > 0
  AND NOT (ag.database_id = ANY(gd.database_uids));
```

- **Zero rows**: nothing to do, close this out.
- **Non-zero rows**: that's the "a real deployment is affected" trigger the loose-ends
  spec asks for. Write a corrective migration that re-points each affected grant at a
  definition whose scope actually covers it (falling through to a synthesized
  definition, the way backfill pass 3 already does, when no existing definition
  matches both shape and scope).

No GitHub issue yet — one should be filed if this ships to production before being
resolved.
