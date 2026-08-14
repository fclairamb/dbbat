-- Two approver kinds, attached to the *database* rather than to the policy
-- shape. Who guards a server is a property of that server, not of the grant
-- definition it happens to be reached through.
--
--   access_approver_user_group_uids — may approve/deny grant *requests*
--                                     targeting this server.
--   query_approver_user_group_uids  — may release approval *holds* on
--                                     statements running against this server.
--
-- Both default to '{}', which is exactly today's behavior: nothing resolves,
-- and only admins decide. There is no hierarchy between the two — a query
-- approver does not inherit access approval, nor the reverse; an org wanting
-- overlap lists the same user group in both columns.
--
-- Resolution is a fallback chain, evaluated live at decision time: the
-- server's own list wins when non-empty, otherwise the union of the lists on
-- the server groups it currently belongs to, otherwise admins.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS access_approver_user_group_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS query_approver_user_group_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

-- The group-level fallback. Like membership, these lists are read live: an
-- edit here immediately changes who may decide, including for grants and holds
-- already in flight. That is deliberate — a departed lead's replacement has to
-- be effective now, not at the next grant issuance.
ALTER TABLE server_groups
    ADD COLUMN IF NOT EXISTS access_approver_user_group_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

ALTER TABLE server_groups
    ADD COLUMN IF NOT EXISTS query_approver_user_group_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

-- "Is this user an approver anywhere?" is asked on every approvals/pending
-- subscribe and re-check, so both overlap probes get a GIN index.
CREATE INDEX IF NOT EXISTS servers_access_approver_groups_idx
    ON servers USING GIN (access_approver_user_group_uids);

--bun:split

CREATE INDEX IF NOT EXISTS servers_query_approver_groups_idx
    ON servers USING GIN (query_approver_user_group_uids);

--bun:split

CREATE INDEX IF NOT EXISTS server_groups_access_approver_groups_idx
    ON server_groups USING GIN (access_approver_user_group_uids);

--bun:split

CREATE INDEX IF NOT EXISTS server_groups_query_approver_groups_idx
    ON server_groups USING GIN (query_approver_user_group_uids);
