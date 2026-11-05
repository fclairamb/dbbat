-- A server name is unique among live rows only, like users_username_active_uq.
-- servers_name_key covered soft-deleted rows too, so deleting a server kept its
-- name taken forever (there is no restore and no purge), and a re-provisioning
-- script could never declare the same database again under its own name.
-- Every lookup by name goes through the soft_delete model, which already
-- excludes deleted rows, so a live and a deleted row sharing a name is never
-- ambiguous.
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_name_key;

--bun:split

CREATE UNIQUE INDEX servers_name_active_uq
    ON servers(name)
    WHERE deleted_at IS NULL;
