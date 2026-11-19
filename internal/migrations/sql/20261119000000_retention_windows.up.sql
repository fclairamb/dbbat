-- Retention windows per level (instance, server group, server), in seconds.
--
-- Three states per column: NULL inherits, 0 means never, a positive value is
-- the duration. Nothing sets a window here: every value starts NULL, and NULL
-- at every level resolves to "never", so an upgraded instance archives nothing
-- and deletes nothing until an operator says so.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS archive_after_seconds bigint,
    ADD COLUMN IF NOT EXISTS delete_after_seconds bigint;

--bun:split

ALTER TABLE server_groups
    ADD COLUMN IF NOT EXISTS archive_after_seconds bigint,
    ADD COLUMN IF NOT EXISTS delete_after_seconds bigint;

--bun:split

-- Archive state of a closed connection. Only the captured result rows are
-- archived (one Parquet object per connection); statements stay in PostgreSQL
-- until delete_after. archive_claimed_at is a lease, so replicas never archive
-- the same connection at once.
ALTER TABLE connections
    ADD COLUMN IF NOT EXISTS archive_claimed_at timestamptz,
    ADD COLUMN IF NOT EXISTS archived_at timestamptz,
    ADD COLUMN IF NOT EXISTS archive_key text,
    ADD COLUMN IF NOT EXISTS archive_rows_sha256 text,
    ADD COLUMN IF NOT EXISTS archive_rows_bytes bigint,
    ADD COLUMN IF NOT EXISTS archive_rows_count bigint,
    ADD COLUMN IF NOT EXISTS archive_deleted_at timestamptz;

--bun:split

-- Candidates of the archive pass: closed, not archived yet.
CREATE INDEX IF NOT EXISTS connections_archive_candidates_idx
    ON connections (disconnected_at)
    WHERE disconnected_at IS NOT NULL AND archived_at IS NULL;
