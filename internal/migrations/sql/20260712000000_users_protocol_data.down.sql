-- Drops the per-user protocol material (Oracle O5LOGON user salts). API keys
-- created with user-salt verifiers keep working for challenge generation (the
-- salts are duplicated in each key's protocol_data), but new keys revert to
-- per-key random salts.
ALTER TABLE users DROP COLUMN protocol_data;
