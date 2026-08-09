ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS database_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

-- Rebuild each definition's per-database scope from the *current* membership
-- of the server groups it points at. That is the best reconstruction available
-- and it is behaviour-preserving: the pre-rollback scope was exactly "the
-- databases currently in these groups".
UPDATE grant_definitions gd
SET database_uids = COALESCE((
    SELECT ARRAY(
        SELECT DISTINCT m.server_uid
        FROM server_group_members m
        WHERE m.group_uid = ANY (gd.server_group_uids)
    )
), '{}')
WHERE cardinality(gd.server_group_uids) > 0;

--bun:split

-- The auto-created groups are left in place: they are ordinary server groups
-- now, possibly edited, and dropping them would destroy an operator's work.
ALTER TABLE grant_definitions DROP COLUMN IF EXISTS server_group_uids;
