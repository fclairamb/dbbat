ALTER TABLE databases ADD COLUMN protocol TEXT NOT NULL DEFAULT 'postgresql';

--bun:split

ALTER TABLE databases ADD COLUMN oracle_service_name TEXT;
