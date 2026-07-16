-- Rename the "databases" table to "servers": the table now also holds SSH
-- bastion rows (protocol = 'ssh'), so "databases" is a misnomer. An SSH bastion
-- and a database are both "a server dbbat knows how to reach" (host, port,
-- username, secret), with the existing protocol column as discriminator.
--
-- The via_uid self-reference means "dial this server through that one" (an SSH
-- bastion), enabling transparent SSH tunneling and multi-hop jump chains.
-- database_name and ssl_mode become nullable because they are meaningless for
-- an SSH server row.
--
-- FK columns on other tables keep the name database_id: they legitimately point
-- at database *targets*, never at bastions.

ALTER TABLE databases RENAME TO servers;

ALTER INDEX idx_databases_name RENAME TO idx_servers_name;
ALTER INDEX idx_databases_deleted_at RENAME TO idx_servers_deleted_at;

ALTER TABLE servers RENAME CONSTRAINT databases_pkey TO servers_pkey;
ALTER TABLE servers RENAME CONSTRAINT databases_name_key TO servers_name_key;

ALTER TABLE servers ADD COLUMN via_uid UUID REFERENCES servers(uid);

ALTER TABLE servers ALTER COLUMN database_name DROP NOT NULL;
ALTER TABLE servers ALTER COLUMN ssl_mode DROP NOT NULL;
