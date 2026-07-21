CREATE TABLE user_groups (
    uid         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now()
);

--bun:split

CREATE UNIQUE INDEX user_groups_name_uniq ON user_groups (lower(name));

--bun:split

CREATE TABLE user_group_members (
    group_uid uuid NOT NULL REFERENCES user_groups(uid) ON DELETE CASCADE,
    user_uid  uuid NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    PRIMARY KEY (group_uid, user_uid)
);

--bun:split

CREATE INDEX user_group_members_user_idx ON user_group_members (user_uid);

--bun:split

-- Scope is stored as UUID arrays on the definition itself (not a join table)
-- so that deleting a group cannot silently empty a definition's scope: an
-- empty scope means "everyone", so a cascade there would fail *open*. A
-- dangling group uid in the array simply matches no user — fail closed.
ALTER TABLE grant_definitions
    ADD COLUMN group_uids    uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN database_uids uuid[] NOT NULL DEFAULT '{}';
