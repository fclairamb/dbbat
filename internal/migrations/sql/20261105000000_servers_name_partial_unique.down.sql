-- Fails if a live row reuses the name of a soft-deleted one: rename or purge
-- one of them before rolling back.
DROP INDEX IF EXISTS servers_name_active_uq;

--bun:split

ALTER TABLE servers ADD CONSTRAINT servers_name_key UNIQUE (name);
