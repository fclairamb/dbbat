ALTER TABLE api_keys ADD COLUMN o5logon_salt BYTEA;
ALTER TABLE api_keys ADD COLUMN o5logon_verifier BYTEA;

--bun:split

-- Backfill note: existing API keys won't have O5LOGON verifiers.
-- They'll work for REST API and PG proxy but not Oracle proxy.
-- Users must regenerate keys to get Oracle support.
