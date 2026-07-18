-- Reverse the databases -> servers rename. Any SSH server rows (protocol='ssh')
-- and via_uid references must be removed before rolling back, since the old
-- schema cannot represent them (database_name/ssl_mode become NOT NULL again).

DELETE FROM servers WHERE protocol = 'ssh';

ALTER TABLE servers ALTER COLUMN ssl_mode SET NOT NULL;
ALTER TABLE servers ALTER COLUMN database_name SET NOT NULL;

ALTER TABLE servers DROP COLUMN via_uid;

ALTER TABLE servers RENAME CONSTRAINT servers_name_key TO databases_name_key;
ALTER TABLE servers RENAME CONSTRAINT servers_pkey TO databases_pkey;

ALTER INDEX idx_servers_deleted_at RENAME TO idx_databases_deleted_at;
ALTER INDEX idx_servers_name RENAME TO idx_databases_name;

ALTER TABLE servers RENAME TO databases;
