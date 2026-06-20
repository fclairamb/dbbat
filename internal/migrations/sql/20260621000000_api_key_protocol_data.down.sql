-- Restore the dedicated verifier-6949 columns and migrate the bytes back out of
-- protocol_data. verifier-18453 material (jsonb-only) is intentionally dropped on
-- rollback — it never had dedicated columns.
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
