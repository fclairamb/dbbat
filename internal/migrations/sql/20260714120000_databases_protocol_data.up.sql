-- Generic per-database protocol-specific settings, mirroring users.protocol_data
-- and api_keys.protocol_data — a single jsonb column instead of a dedicated
-- column per protocol-specific setting. First consumer: the MongoDB proxy's
-- upstream SCRAM authSource (protocol_data.mongodb.auth_source), historically
-- fixed to "admin" (the MongoDB default, e.g. MONGO_INITDB_ROOT_USERNAME);
-- absent/empty keeps the "admin" default. oracle_service_name predates this
-- convention and stays a dedicated column.
ALTER TABLE databases ADD COLUMN protocol_data jsonb;
