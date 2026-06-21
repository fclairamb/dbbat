-- Restore the dedicated verifier-6949 columns and migrate the bytes back out of
-- protocol_data, reversing to the canonical pre-state (the schema on main). The
-- verifier-18453 columns are intentionally NOT recreated — no retained migration
-- defines them — so any verifier-18453 material is dropped on rollback.
ALTER TABLE api_keys ADD COLUMN o5logon_salt BYTEA;

--bun:split

ALTER TABLE api_keys ADD COLUMN o5logon_verifier BYTEA;

--bun:split

UPDATE api_keys
SET o5logon_salt     = decode(protocol_data->'oracle'->>'o5logon_salt_6949', 'base64'),
    o5logon_verifier = decode(protocol_data->'oracle'->>'o5logon_verifier_6949', 'base64')
WHERE protocol_data->'oracle'->>'o5logon_verifier_6949' IS NOT NULL;

--bun:split

ALTER TABLE api_keys DROP COLUMN protocol_data;
