ALTER TABLE grant_definitions RENAME COLUMN approver_user_group_uids TO approver_group_uids;

--bun:split

ALTER TABLE grant_definitions RENAME COLUMN user_group_uids TO group_uids;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS server_group_uid;

--bun:split

DROP TABLE IF EXISTS server_group_members;

--bun:split

DROP TABLE IF EXISTS server_groups;
