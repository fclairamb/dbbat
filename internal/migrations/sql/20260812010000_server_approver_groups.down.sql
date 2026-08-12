DROP INDEX IF EXISTS server_groups_query_approver_groups_idx;

--bun:split

DROP INDEX IF EXISTS server_groups_access_approver_groups_idx;

--bun:split

DROP INDEX IF EXISTS servers_query_approver_groups_idx;

--bun:split

DROP INDEX IF EXISTS servers_access_approver_groups_idx;

--bun:split

ALTER TABLE server_groups DROP COLUMN IF EXISTS query_approver_user_group_uids;

--bun:split

ALTER TABLE server_groups DROP COLUMN IF EXISTS access_approver_user_group_uids;

--bun:split

ALTER TABLE servers DROP COLUMN IF EXISTS query_approver_user_group_uids;

--bun:split

ALTER TABLE servers DROP COLUMN IF EXISTS access_approver_user_group_uids;
