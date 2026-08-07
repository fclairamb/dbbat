-- Grants become instances of a grant definition and stop carrying any shape
-- data of their own.
--
-- Until now an access grant copied the whole behavioral shape (controls,
-- quotas, approval patterns, approver groups) onto its own row, because an
-- admin could invent a grant from scratch through POST /grants. That made
-- definitions untrustworthy as the policy source of truth, and every new
-- definition feature had to grow a mirror column plus mirror code — a missed
-- mirror being a silent policy hole. From here a grant carries only its
-- instance data (user, database, window, revocation, priority) and points at
-- the definition row it was issued from.
--
-- Definitions are immutably versioned: editing one archives the current row
-- and inserts a successor, so a grant's behaviour can never change under it.
-- A slug is unique only among live rows.
ALTER TABLE grant_definitions ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

--bun:split

-- lineage_uid ties every version of one definition together. The slug cannot
-- do that job: it is unique only among live rows from now on, so a brand-new
-- definition may legally reuse a slug whose archived rows belong to an
-- entirely unrelated lineage. Deactivation and hard deletion act on a lineage
-- (an operator deactivates *a definition*, not one of its versions), which is
-- exactly what makes deactivation reach the grants pinned to older versions.
ALTER TABLE grant_definitions ADD COLUMN IF NOT EXISTS lineage_uid UUID;

--bun:split

-- Every pre-existing definition is the root of its own lineage.
UPDATE grant_definitions SET lineage_uid = uid WHERE lineage_uid IS NULL;

--bun:split

ALTER TABLE grant_definitions ALTER COLUMN lineage_uid SET NOT NULL;

--bun:split

CREATE INDEX IF NOT EXISTS idx_grant_definitions_lineage ON grant_definitions(lineage_uid);

--bun:split

-- The slug was globally unique (a UNIQUE constraint, added in
-- 20260805000000). Versioning makes that impossible: an edit produces a second
-- row with the same slug. Replace it with a partial unique index so exactly
-- one *live* row owns a slug and any number of archived ones may share it.
ALTER TABLE grant_definitions DROP CONSTRAINT IF EXISTS grant_definitions_slug_uniq;

--bun:split

CREATE UNIQUE INDEX grant_definitions_live_slug_uniq
    ON grant_definitions(slug) WHERE archived_at IS NULL;

--bun:split

-- Same problem for the unique-active-name index: an archived version and its
-- successor share a name and are both is_active, so the very first edit of any
-- definition would violate it. Narrow it to live rows.
DROP INDEX IF EXISTS grant_definitions_active_name_uniq;

--bun:split

CREATE UNIQUE INDEX grant_definitions_active_name_uniq
    ON grant_definitions(name) WHERE is_active AND archived_at IS NULL;

--bun:split

-- Nullable first: the backfill below has to run before the column can be
-- locked down, and NOT NULL up front would fail outright on any existing row.
ALTER TABLE access_grants
    ADD COLUMN IF NOT EXISTS grant_definition_id UUID REFERENCES grant_definitions(uid);

--bun:split

-- Backfill pass 1 — provenance. grant_requests.resulting_grant_id names the
-- grant a definition materialized, so for those grants the link is recorded
-- fact rather than inference. Run first so nothing else gets a chance to guess
-- at a grant whose origin is known.
UPDATE access_grants AS ag
SET grant_definition_id = gr.grant_definition_id
FROM grant_requests AS gr
WHERE gr.resulting_grant_id = ag.uid
  AND ag.grant_definition_id IS NULL;

--bun:split

-- Backfill pass 2 — shape match. A grant whose shape is exactly some active
-- definition's shape is (almost certainly) an instance of it, so point it
-- there rather than synthesizing a duplicate.
--
--   * Arrays compare *sorted* (ARRAY(SELECT unnest(x) ORDER BY 1)): element
--     order carries no meaning for controls, patterns or approver groups, so
--     comparing raw arrays would report false mismatches.
--   * Quotas compare with IS NOT DISTINCT FROM so NULL (unlimited) equals
--     NULL instead of yielding NULL.
--   * duration_seconds is deliberately NOT part of the match: the validity
--     window lives on the grant and is untouched by this migration, so a
--     definition whose duration differs describes the same behaviour.
--   * Only active definitions are candidates — matching an inactive one would
--     silently stop the grant authorising anything.
--   * ORDER BY created_at, uid makes the pick deterministic when several
--     definitions share a shape.
--
-- Database scope IS part of the match: a definition is a candidate only when
-- it is unscoped (empty database_uids — it covers every database) or its
-- database_uids lists the grant's own database_id. Grants left matching
-- nothing fall through to pass 3's synthesis, which is already their correct
-- destination.
--
-- Without that condition a grant could be pinned to a definition whose scope
-- excludes the grant's own database. That is behaviourally inert — auth never
-- consults a definition's database scope, only the assign endpoint does, at
-- issuance time — but such a grant could not be re-issued through the assign
-- endpoint, a real if narrow inconsistency. It is not hypothetical: on
-- 2026-08-07, before this migration had shipped in any tagged release or
-- reached production, this pass was replayed read-only against the production
-- dbbat database and predicted 10 grants across 4 distinct databases being
-- pinned to a definition that excludes them. So it is fixed here rather than
-- documented (see
-- specs/todos/2026-08-07-verify-grant-backfill-scope-before-real-deploy.md).
-- Environments that already ran the earlier version of this file are repaired
-- by 20260807010000_grants_rescope_mismatched_definitions, which is a no-op
-- everywhere else.
--
-- cardinality() and not array_length(): array_length(x, 1) yields NULL for an
-- empty array, which would make the "unscoped" test NULL rather than true and
-- send every unscoped definition — the common case — out of the candidate set.
-- The column is NOT NULL DEFAULT '{}'; the IS NULL arm is belt and braces.
UPDATE access_grants AS ag
SET grant_definition_id = (
    SELECT gd.uid
    FROM grant_definitions AS gd
    WHERE gd.is_active
      AND (gd.database_uids IS NULL
        OR cardinality(gd.database_uids) = 0
        OR ag.database_id = ANY (gd.database_uids))
      AND ARRAY(SELECT unnest(gd.controls) ORDER BY 1)
        = ARRAY(SELECT unnest(ag.controls) ORDER BY 1)
      AND gd.max_query_counts IS NOT DISTINCT FROM ag.max_query_counts
      AND gd.max_bytes_transferred IS NOT DISTINCT FROM ag.max_bytes_transferred
      AND ARRAY(SELECT unnest(gd.approval_patterns) ORDER BY 1)
        = ARRAY(SELECT unnest(ag.approval_patterns) ORDER BY 1)
      AND ARRAY(SELECT unnest(gd.approver_group_uids) ORDER BY 1)
        = ARRAY(SELECT unnest(ag.approver_group_uids) ORDER BY 1)
    ORDER BY gd.created_at, gd.uid
    LIMIT 1
)
WHERE ag.grant_definition_id IS NULL;

--bun:split

-- Backfill pass 3 — synthesis. Whatever is left was an ad-hoc admin grant
-- whose shape matches no definition. Every *distinct* remaining shape gets one
-- synthesized definition (duplicates collapse onto it by construction: the
-- GROUP BY is the shape itself), and the grants carrying that shape point at
-- it.
--
-- The synthesized rows are is_active = FALSE on purpose. They are a record of
-- what an ad-hoc grant used to be, not a policy anybody should be able to
-- issue from; the 'legacy-grant-shape-' slug prefix says so out loud. Because
-- auth checks is_active, those legacy grants stop authorising new connections
-- — the fail-closed direction, and called out in the release notes.
--
-- new_uid is generated in its own CTE and used for both uid and lineage_uid
-- (a synthesized definition is the root of its own lineage). The CTE is
-- MATERIALIZED so the volatile gen_random_uuid() is evaluated exactly once
-- per row rather than once per reference.
WITH shapes AS (
    SELECT
        ARRAY(SELECT unnest(ag.controls) ORDER BY 1)            AS k_controls,
        ag.max_query_counts                                     AS k_max_queries,
        ag.max_bytes_transferred                                AS k_max_bytes,
        ARRAY(SELECT unnest(ag.approval_patterns) ORDER BY 1)   AS k_patterns,
        ARRAY(SELECT unnest(ag.approver_group_uids) ORDER BY 1) AS k_approvers,
        -- The definition needs a duration (CHECK duration_seconds > 0). The
        -- longest window any grant of this shape was given is the closest
        -- honest answer; GREATEST(1, …) keeps a degenerate window legal.
        GREATEST(1, MAX(CEIL(EXTRACT(EPOCH FROM (ag.expires_at - ag.starts_at)))::BIGINT)) AS duration_seconds,
        (array_agg(ag.granted_by ORDER BY ag.created_at, ag.uid))[1] AS created_by
    FROM access_grants AS ag
    WHERE ag.grant_definition_id IS NULL
    GROUP BY 1, 2, 3, 4, 5
), numbered AS MATERIALIZED (
    SELECT
        s.*,
        gen_random_uuid() AS new_uid,
        row_number() OVER (
            ORDER BY s.k_controls, s.k_max_queries, s.k_max_bytes, s.k_patterns, s.k_approvers
        ) AS rn
    FROM shapes AS s
), inserted AS (
    INSERT INTO grant_definitions (
        uid, lineage_uid, name, slug, description, duration_seconds,
        controls, max_query_counts, max_bytes_transferred,
        approval_patterns, approver_group_uids, is_active, created_by
    )
    SELECT
        n.new_uid,
        n.new_uid,
        'Legacy grant shape ' || n.rn::TEXT,
        'legacy-grant-shape-' || left(n.new_uid::TEXT, 8) || '-' || n.rn::TEXT,
        'Synthesized from ad-hoc grants that predate definition-backed grants. '
            || 'Inactive: kept so existing grants keep a readable shape, not to issue from.',
        n.duration_seconds,
        n.k_controls,
        n.k_max_queries,
        n.k_max_bytes,
        n.k_patterns,
        n.k_approvers,
        FALSE,
        n.created_by
    FROM numbered AS n
    RETURNING uid, controls, max_query_counts, max_bytes_transferred,
              approval_patterns, approver_group_uids
)
UPDATE access_grants AS ag
SET grant_definition_id = i.uid
FROM inserted AS i
WHERE ag.grant_definition_id IS NULL
  AND ARRAY(SELECT unnest(ag.controls) ORDER BY 1) = i.controls
  AND ag.max_query_counts IS NOT DISTINCT FROM i.max_query_counts
  AND ag.max_bytes_transferred IS NOT DISTINCT FROM i.max_bytes_transferred
  AND ARRAY(SELECT unnest(ag.approval_patterns) ORDER BY 1) = i.approval_patterns
  AND ARRAY(SELECT unnest(ag.approver_group_uids) ORDER BY 1) = i.approver_group_uids;

--bun:split

-- Every grant now has a definition, so the link can be made mandatory. If this
-- statement fails, the backfill above left a row behind and the migration must
-- stop rather than ship a half-linked table.
ALTER TABLE access_grants ALTER COLUMN grant_definition_id SET NOT NULL;

--bun:split

CREATE INDEX IF NOT EXISTS idx_access_grants_definition ON access_grants(grant_definition_id);

--bun:split

-- The point of the whole change: a grant carries no behavioral shape. priority
-- stays — it ranks a grant against the user's other overlapping grants
-- (instance-level selection), it is not policy.
ALTER TABLE access_grants DROP COLUMN IF EXISTS controls;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS max_query_counts;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS max_bytes_transferred;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS approval_patterns;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS approver_group_uids;

--bun:split

COMMENT ON COLUMN access_grants.grant_definition_id IS
    'The exact grant_definitions row this grant was issued from. Definitions are immutably versioned, so this pins the version: an edit archives the row and inserts a successor, never changing this grant''s behaviour.';
