-- Grant definitions now scope on server *groups* instead of enumerating
-- individual servers, so a policy names a stable set and fleet growth stops
-- being O(definitions) of editing.
ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS server_group_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

-- Mirror every existing per-database scope into a real server group, so no
-- definition changes behavior: the group holds exactly the servers the
-- definition used to list.
--
-- One group per *distinct set* of databases, not one per definition row:
-- definitions are immutably versioned, so several rows of one lineage
-- typically share the same scope, and one group per row would litter the
-- fleet with near-duplicates.
--
-- Idempotent by construction. The guard is `cardinality(server_group_uids) = 0
-- AND cardinality(database_uids) > 0`, so a re-run after a partial failure
-- picks up exactly what is left and a run with nothing to do does nothing.
-- The whole body is skipped when database_uids is already gone (the next
-- statement drops it), which is what makes a retry safe at any point: PL/pgSQL
-- resolves column names when a statement first executes, not when the block is
-- entered, so the un-taken branch never has to parse.
DO $$
DECLARE
    scope        uuid[];
    new_group    uuid;
    base_name    text;
    final_name   text;
    attempt      int;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'grant_definitions' AND column_name = 'database_uids'
    ) THEN
        RETURN;
    END IF;

    FOR scope IN
        SELECT DISTINCT ARRAY(SELECT unnest(gd.database_uids) ORDER BY 1)
        FROM grant_definitions gd
        WHERE cardinality(gd.database_uids) > 0
          AND cardinality(gd.server_group_uids) = 0
    LOOP
        -- Name the group after the definition that first used this scope, so
        -- an admin recognizes it. Names are unique case-insensitively, so fall
        -- back to a numbered suffix rather than failing the migration.
        SELECT gd.name INTO base_name
        FROM grant_definitions gd
        WHERE ARRAY(SELECT unnest(gd.database_uids) ORDER BY 1) = scope
        ORDER BY gd.created_at, gd.uid
        LIMIT 1;

        base_name := left(coalesce(base_name, 'scope'), 48) || ' servers';
        final_name := base_name;
        attempt := 1;

        WHILE EXISTS (SELECT 1 FROM server_groups sg WHERE lower(sg.name) = lower(final_name)) LOOP
            attempt := attempt + 1;
            final_name := base_name || ' ' || attempt::text;
        END LOOP;

        INSERT INTO server_groups (name, description)
        VALUES (
            final_name,
            'Created automatically when grant definitions moved from per-server scoping to server groups.'
        )
        RETURNING uid INTO new_group;

        -- A scope entry naming a server that no longer exists is dropped
        -- rather than resurrected: it already matched nothing.
        INSERT INTO server_group_members (group_uid, server_uid)
        SELECT new_group, s.uid
        FROM unnest(scope) AS s(uid)
        WHERE EXISTS (SELECT 1 FROM servers srv WHERE srv.uid = s.uid)
        ON CONFLICT (group_uid, server_uid) DO NOTHING;

        UPDATE grant_definitions gd
        SET server_group_uids = ARRAY[new_group]
        WHERE ARRAY(SELECT unnest(gd.database_uids) ORDER BY 1) = scope
          AND cardinality(gd.server_group_uids) = 0;
    END LOOP;
END
$$;

--bun:split

-- Only now, with every scope mirrored into a group above, does the old column
-- go. The down migration rebuilds it from the group memberships.
ALTER TABLE grant_definitions DROP COLUMN IF EXISTS database_uids;
