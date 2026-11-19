DROP INDEX IF EXISTS connections_archive_candidates_idx;

--bun:split

ALTER TABLE connections
    DROP COLUMN IF EXISTS archive_claimed_at,
    DROP COLUMN IF EXISTS archived_at,
    DROP COLUMN IF EXISTS archive_key,
    DROP COLUMN IF EXISTS archive_rows_sha256,
    DROP COLUMN IF EXISTS archive_rows_bytes,
    DROP COLUMN IF EXISTS archive_rows_count,
    DROP COLUMN IF EXISTS archive_deleted_at;

--bun:split

ALTER TABLE server_groups
    DROP COLUMN IF EXISTS archive_after_seconds,
    DROP COLUMN IF EXISTS delete_after_seconds;

--bun:split

ALTER TABLE servers
    DROP COLUMN IF EXISTS archive_after_seconds,
    DROP COLUMN IF EXISTS delete_after_seconds;
