DROP INDEX IF EXISTS idx_connections_terminate_requested;

--bun:split

ALTER TABLE connections
    DROP COLUMN IF EXISTS terminate_requested_at,
    DROP COLUMN IF EXISTS terminate_requested_by,
    DROP COLUMN IF EXISTS terminate_reason;
