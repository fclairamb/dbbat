-- Generic per-user protocol-specific material, mirroring api_keys.protocol_data.
-- First occupant: the per-USER O5LOGON salts (o5logon_user_salt_6949 /
-- o5logon_user_salt_18453) under an "oracle" key. Sharing one salt pair across
-- all of a user's API keys lets the Oracle proxy commit to a single salt in the
-- AUTH challenge while keeping every user-salt key as a login candidate —
-- lifting the "only the first verifier-bearing API key works for Oracle" limit.
-- Populated lazily at API key creation; NULL until then.
ALTER TABLE users ADD COLUMN protocol_data jsonb;
