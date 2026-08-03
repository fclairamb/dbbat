DROP INDEX IF EXISTS idx_connections_instance_id_open;

--bun:split

ALTER TABLE connections DROP COLUMN IF EXISTS instance_id;
