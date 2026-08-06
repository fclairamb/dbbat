-- Reverses the up-migration: shape data comes back onto access_grants, the
-- definition link goes away, versioning is undone.
--
-- Order matters throughout: the shape has to be copied back off the
-- definitions *before* the FK column is dropped, and the synthesized
-- definitions can only be deleted *after* it, since they are referenced by it.
ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS controls TEXT[] NOT NULL DEFAULT '{}';

--bun:split

ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS max_query_counts INT;

--bun:split

ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS max_bytes_transferred BIGINT;

--bun:split

ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS approval_patterns TEXT[] NOT NULL DEFAULT '{}';

--bun:split

ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS approver_group_uids UUID[] NOT NULL DEFAULT '{}';

--bun:split

-- Re-mirror each grant's shape from the definition it was pinned to, which is
-- exactly what the pre-change code would have stored. max_query_counts is
-- narrower here (INT) than on definitions (BIGINT), so it is clamped rather
-- than allowed to overflow.
UPDATE access_grants AS ag
SET controls              = gd.controls,
    max_query_counts      = LEAST(gd.max_query_counts, 2147483647)::INT,
    max_bytes_transferred = gd.max_bytes_transferred,
    approval_patterns     = gd.approval_patterns,
    approver_group_uids   = gd.approver_group_uids
FROM grant_definitions AS gd
WHERE gd.uid = ag.grant_definition_id;

--bun:split

CREATE INDEX IF NOT EXISTS idx_access_grants_controls ON access_grants USING GIN(controls);

--bun:split

DROP INDEX IF EXISTS idx_access_grants_definition;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS grant_definition_id;

--bun:split

-- The synthesized stand-ins have no meaning once grants carry their own shape
-- again. Nothing else ever references them (they were created by this
-- migration and only access_grants pointed at them), so they can go.
DELETE FROM grant_definitions WHERE slug LIKE 'legacy-grant-shape-%';

--bun:split

-- Archived versions cannot survive the restored global-unique slug constraint,
-- so the lineage collapses back to its live row. Grant requests pinned to an
-- archived version are repointed at that live row first, otherwise the FK
-- blocks the delete.
UPDATE grant_requests AS gr
SET grant_definition_id = live.uid
FROM grant_definitions AS old
JOIN grant_definitions AS live
  ON live.lineage_uid = old.lineage_uid AND live.archived_at IS NULL
WHERE gr.grant_definition_id = old.uid
  AND old.archived_at IS NOT NULL;

--bun:split

DELETE FROM grant_definitions WHERE archived_at IS NOT NULL;

--bun:split

DROP INDEX IF EXISTS grant_definitions_live_slug_uniq;

--bun:split

ALTER TABLE grant_definitions ADD CONSTRAINT grant_definitions_slug_uniq UNIQUE (slug);

--bun:split

DROP INDEX IF EXISTS grant_definitions_active_name_uniq;

--bun:split

CREATE UNIQUE INDEX grant_definitions_active_name_uniq
    ON grant_definitions(name) WHERE is_active;

--bun:split

DROP INDEX IF EXISTS idx_grant_definitions_lineage;

--bun:split

ALTER TABLE grant_definitions DROP COLUMN IF EXISTS lineage_uid;

--bun:split

ALTER TABLE grant_definitions DROP COLUMN IF EXISTS archived_at;
