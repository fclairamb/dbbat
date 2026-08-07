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

## Resolved open questions

**May automation run this verification query against the real production dbbat
deployment, rather than waiting for a human to do it?**

Decision (2026-08-07, repository owner): **yes — run it against production.**
The query is read-only. Act on the result exactly as the two bullets above say:
zero rows → close this spec out with a note recording the result; non-zero rows →
write the corrective migration described above.

**Should a GitHub issue be filed for this spec?**

Decision: **no.** Do not run `gh issue create`. The spec file is the record.

## Verification result (2026-08-07) — NON-ZERO, a real deployment IS affected

Run against the production dbbat storage database (`aws/master`, namespace
`tooling`, DSN read from the `dbbat` secret into a throwaway `postgres:16-alpine`
pod).

**The migration has NOT reached production yet**: `access_grants` has no
`grant_definition_id` column, and `bun_migrations` has no
`grants_reference_definitions` row. So the query in `## Implementation` above
cannot run as written — it joins on a column that does not exist there yet.

Instead, backfill pass 2's matching logic was **replayed read-only** against
production's current legacy data, to predict what the migration *would* do when
it lands. Baseline: 98 grants, 4 active definitions (1 of them database-scoped),
71 grants claimed by pass 1 via `grant_requests`.

Excluding the grants pass 1 claims first, pass 2 would pin

> **10 grants, spanning 4 distinct databases**, to the definition `abyla-rw-30d`,
> whose `database_uids` does **not** contain the grant's own `database_id`.

That is exactly the "non-zero rows" branch above, and exactly the "a real
deployment is affected" trigger the loose-ends spec asked for. The asymmetry is
no longer hypothetical, so it must be fixed before this migration is deployed.

Affected grant uids (`database_id` → matched definition `abyla-rw-30d`):

```
37dd1f0b-9244-43d9-a40d-e42656f9c135   a400cbfa-f9d3-4a58-8099-806e42bcf87e
a6951dae-bafe-44fe-93d8-15e09094196d   7a9895b1-cbed-4edc-87bf-640726876c0d
07f7b52d-adf6-4a5f-b46a-14cf058e136d   2f8b8af9-d11b-4bff-a810-54ab77f5e82d
f01c3866-fae1-4e5b-b408-6e7658ff4ac8   381c3ae9-58d2-486e-b569-152ce3bb2be4
99633ac6-4a88-445a-a5ee-e84aa5528ad2   381c3ae9-58d2-486e-b569-152ce3bb2be4
89db3f63-9a60-4ec5-90fc-971cd335a522   a400cbfa-f9d3-4a58-8099-806e42bcf87e
b6914dba-4cd3-4076-a085-0ed1658237c0   a400cbfa-f9d3-4a58-8099-806e42bcf87e
ed2ef17a-c532-4e52-abb7-3264a7795a16   381c3ae9-58d2-486e-b569-152ce3bb2be4
f6d4cac9-1f98-4bf4-a61b-20220b89ee8e   a400cbfa-f9d3-4a58-8099-806e42bcf87e
1b17bfbe-0d76-4f4f-bee6-8a9366af9c76   a400cbfa-f9d3-4a58-8099-806e42bcf87e
```

### What to build

Because the migration has **not** shipped in any tagged release *and* has not run
on production, the cleanest fix is to **correct pass 2 in place** rather than
layer a corrective migration on top of a wrong one:

1. Add the database-scope condition to pass 2's match predicate in
   `internal/migrations/sql/20260806020000_grants_reference_definitions.up.sql`
   — a definition is only a candidate if its `database_uids` is empty
   (unscoped, matches anything) **or** contains the grant's `database_id`.
   Grants that then match nothing fall through to pass 3's synthesis, which is
   already the correct destination for them.
2. Replace the long "Deliberately NOT part of the match: gd.database_uids"
   comment block with one describing the new, correct behaviour, and note that
   the asymmetry was found against real production data before the migration
   shipped.
3. **Environments that already applied the original migration** (the shared dev
   database is the known one) will not re-run an amended file. Add a *separate*,
   small corrective migration that re-points any already-backfilled grant whose
   definition scope excludes its database, using the same fall-through-to-pass-3
   rule. Make it a no-op where nothing is affected, so it is safe everywhere.
4. Cover it in `internal/store/grants_definition_migration_test.go`, which
   already exercises this migration: add a case where a shape-matching
   definition's `database_uids` excludes the grant's database, and assert the
   grant does NOT get pinned to it.
