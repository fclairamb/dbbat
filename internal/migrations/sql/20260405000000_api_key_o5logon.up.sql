-- Backfill note: existing API keys won't have O5LOGON verifiers.
-- They'll work for REST API and PG proxy but not Oracle proxy.
-- Users must regenerate keys to get Oracle support.
ALTER TABLE api_keys ADD COLUMN o5logon_salt BYTEA;

--bun:split

ALTER TABLE api_keys ADD COLUMN o5logon_verifier BYTEA;
