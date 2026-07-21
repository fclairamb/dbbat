ALTER TABLE grant_definitions
    DROP COLUMN IF EXISTS group_uids,
    DROP COLUMN IF EXISTS database_uids;

--bun:split

DROP TABLE IF EXISTS user_group_members;

--bun:split

DROP TABLE IF EXISTS user_groups;
