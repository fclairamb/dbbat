-- Repairs grants that 20260806020000_grants_reference_definitions pinned to a
-- definition whose database scope excludes the grant's own database.
--
-- That migration's backfill pass 2 originally matched a legacy grant to an
-- active definition on shape alone, ignoring the definition's database_uids.
-- Pass 2 has since been corrected in place, but an environment that already
-- applied the earlier version of the file will never re-run it (bun records
-- the migration by name, not by content) — the shared dev database is the
-- known case. This migration is that environment's repair, and a strict no-op
-- anywhere the mismatch does not exist, which is why it is safe to run
-- everywhere: production applies the corrected pass 2 and then finds nothing
-- to do here.
--
-- Scope of the repair: grants whose link is an *inference*. A grant named by
-- grant_requests.resulting_grant_id was linked by backfill pass 1 from
-- recorded provenance — it really was materialized from that definition, even
-- if the definition's scope was narrowed afterwards — so rewriting that link
-- would destroy a fact. Those are left alone. Pass 3's synthesized definitions
-- are unscoped, so they never trip the predicate in the first place.
--
-- cardinality() and not array_length(): array_length(x, 1) is NULL for an
-- empty array, so it cannot express "unscoped" without extra NULL handling.

-- Step 1 — re-point onto a definition that matches both shape and scope.
-- Same shape comparison as pass 2 (arrays sorted, quotas IS NOT DISTINCT
-- FROM), and the shape is read off the definition the grant is wrongly pinned
-- to: pass 2 only ever matched on an exact shape equality, so that definition
-- carries the grant's original shape verbatim.
WITH mismatched AS (
    SELECT
        ag.uid          AS grant_uid,
        ag.database_id  AS database_id,
        cur.uid         AS current_definition_uid,
        cur.controls,
        cur.max_query_counts,
        cur.max_bytes_transferred,
        cur.approval_patterns,
        cur.approver_group_uids
    FROM access_grants AS ag
    JOIN grant_definitions AS cur ON cur.uid = ag.grant_definition_id
    WHERE cardinality(cur.database_uids) > 0
      AND NOT (ag.database_id = ANY (cur.database_uids))
      AND NOT EXISTS (
          SELECT 1 FROM grant_requests AS gr WHERE gr.resulting_grant_id = ag.uid
      )
), rescoped AS (
    SELECT
        m.grant_uid,
        (
            SELECT alt.uid
            FROM grant_definitions AS alt
            WHERE alt.is_active
              AND alt.archived_at IS NULL
              AND alt.uid <> m.current_definition_uid
              AND (cardinality(alt.database_uids) = 0
                OR m.database_id = ANY (alt.database_uids))
              AND ARRAY(SELECT unnest(alt.controls) ORDER BY 1)
                = ARRAY(SELECT unnest(m.controls) ORDER BY 1)
              AND alt.max_query_counts IS NOT DISTINCT FROM m.max_query_counts
              AND alt.max_bytes_transferred IS NOT DISTINCT FROM m.max_bytes_transferred
              AND ARRAY(SELECT unnest(alt.approval_patterns) ORDER BY 1)
                = ARRAY(SELECT unnest(m.approval_patterns) ORDER BY 1)
              AND ARRAY(SELECT unnest(alt.approver_group_uids) ORDER BY 1)
                = ARRAY(SELECT unnest(m.approver_group_uids) ORDER BY 1)
            ORDER BY alt.created_at, alt.uid
            LIMIT 1
        ) AS new_definition_uid
    FROM mismatched AS m
)
UPDATE access_grants AS ag
SET grant_definition_id = r.new_definition_uid
FROM rescoped AS r
WHERE ag.uid = r.grant_uid
  AND r.new_definition_uid IS NOT NULL;

--bun:split

-- Step 2 — synthesis, the same fall-through pass 3 applies. Whatever step 1
-- could not re-point has no in-scope definition to go to, so each distinct
-- remaining shape gets one inactive stand-in, exactly like an ad-hoc grant
-- that never matched anything.
--
-- The predicate is recomputed rather than carried over: a grant re-pointed by
-- step 1 no longer satisfies it, and one that was not still does.
--
-- The slug keeps pass 3's 'legacy-grant-shape-' prefix so the down-migration
-- of 20260806020000 — which deletes by that prefix — still cleans these up on
-- a full rollback. '-rescoped-' distinguishes them for a human reading the
-- table. Names collide with nothing: the unique-name index covers active rows
-- only, and these are inactive.
--
-- duration_seconds comes from the definitions being left behind (the longest,
-- when one shape spans several), since the grants no longer carry a shape of
-- their own and their own window was already consumed by pass 3.
WITH mismatched AS (
    SELECT
        ag.uid         AS grant_uid,
        ag.granted_by  AS granted_by,
        ag.created_at  AS created_at,
        cur.duration_seconds,
        ARRAY(SELECT unnest(cur.controls) ORDER BY 1)            AS k_controls,
        cur.max_query_counts                                     AS k_max_queries,
        cur.max_bytes_transferred                                AS k_max_bytes,
        ARRAY(SELECT unnest(cur.approval_patterns) ORDER BY 1)   AS k_patterns,
        ARRAY(SELECT unnest(cur.approver_group_uids) ORDER BY 1) AS k_approvers
    FROM access_grants AS ag
    JOIN grant_definitions AS cur ON cur.uid = ag.grant_definition_id
    WHERE cardinality(cur.database_uids) > 0
      AND NOT (ag.database_id = ANY (cur.database_uids))
      AND NOT EXISTS (
          SELECT 1 FROM grant_requests AS gr WHERE gr.resulting_grant_id = ag.uid
      )
), shapes AS (
    SELECT
        m.k_controls,
        m.k_max_queries,
        m.k_max_bytes,
        m.k_patterns,
        m.k_approvers,
        GREATEST(1, MAX(m.duration_seconds))                          AS duration_seconds,
        (array_agg(m.granted_by ORDER BY m.created_at, m.grant_uid))[1] AS created_by
    FROM mismatched AS m
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
        'Legacy grant shape (rescoped) ' || n.rn::TEXT,
        'legacy-grant-shape-' || left(n.new_uid::TEXT, 8) || '-rescoped-' || n.rn::TEXT,
        'Synthesized for grants an earlier backfill pinned to a definition whose '
            || 'database scope excluded them. Inactive: a record of the shape, not a '
            || 'policy to issue from.',
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
FROM inserted AS i, mismatched AS m
WHERE m.grant_uid = ag.uid
  AND i.controls = m.k_controls
  AND i.max_query_counts IS NOT DISTINCT FROM m.k_max_queries
  AND i.max_bytes_transferred IS NOT DISTINCT FROM m.k_max_bytes
  AND i.approval_patterns = m.k_patterns
  AND i.approver_group_uids = m.k_approvers;
