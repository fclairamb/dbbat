-- Server groups: the named, stable set of database servers that rights are
-- scoped on. A literal mirror of user_groups / user_group_members — same shape,
-- same cascade rules — because the two concepts are symmetric: one names a set
-- of people, the other a set of servers.
CREATE TABLE server_groups (
    uid         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now()
);

--bun:split

CREATE UNIQUE INDEX server_groups_name_uniq ON server_groups (lower(name));

--bun:split

-- Membership is a join table (queried in both directions) and cascading
-- deletes are safe here for the same reason they are on user_group_members:
-- a definition's scope does not live in this table.
CREATE TABLE server_group_members (
    group_uid  uuid NOT NULL REFERENCES server_groups(uid) ON DELETE CASCADE,
    server_uid uuid NOT NULL REFERENCES servers(uid) ON DELETE CASCADE,
    PRIMARY KEY (group_uid, server_uid)
);

--bun:split

-- The auth path asks "which groups is this server in?" on every connection,
-- so the reverse direction needs its own index.
CREATE INDEX server_group_members_server_idx ON server_group_members (server_uid);

--bun:split

-- A grant binds to a server group in addition to its anchor database: it
-- covers whatever the group contains *right now*, so adding a server to the
-- group extends every already-live grant bound to it. NULL — the default and
-- the state of every pre-existing grant — means "anchor database only", which
-- is exactly today's behavior.
--
-- ON DELETE SET NULL rather than CASCADE: deleting a group must narrow those
-- grants back to their anchor, never delete access silently and never widen
-- it.
ALTER TABLE access_grants
    ADD COLUMN server_group_uid uuid REFERENCES server_groups(uid) ON DELETE SET NULL;

--bun:split

CREATE INDEX access_grants_server_group_idx ON access_grants (server_group_uid)
    WHERE server_group_uid IS NOT NULL;

--bun:split

-- Once two kinds of group exist, a bare `group_uids` means nothing. Both
-- columns live only on grant_definitions: 20260806020000 already dropped the
-- access_grants copy of approver_group_uids.
ALTER TABLE grant_definitions RENAME COLUMN group_uids TO user_group_uids;

--bun:split

ALTER TABLE grant_definitions RENAME COLUMN approver_group_uids TO approver_user_group_uids;
