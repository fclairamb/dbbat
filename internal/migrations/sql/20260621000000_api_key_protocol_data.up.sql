-- Consolidate protocol-specific API-key material into a single jsonb column
-- (protocol_data) instead of dedicated per-protocol columns. It currently holds
-- the Oracle O5LOGON verifiers (6949 + 18453). Existing verifier-6949 material is
-- migrated into protocol_data.oracle; keys must be regenerated to gain the
-- verifier-18453 (modern thin-client / sqlplus) material, which is not stored in
-- the dropped columns.
ALTER TABLE api_keys ADD COLUMN protocol_data jsonb;

--bun:split

-- Move existing verifier-6949 bytes in as base64 (matching Go's json []byte
-- encoding). encode(...,'base64') wraps at 76 cols, so strip newlines — Go's
-- base64 decoder rejects them. jsonb_strip_nulls drops absent halves.
UPDATE api_keys
SET protocol_data = jsonb_build_object(
    'oracle',
    jsonb_strip_nulls(jsonb_build_object(
        'o5logon_salt_6949',     replace(encode(o5logon_salt, 'base64'), E'\n', ''),
        'o5logon_verifier_6949', replace(encode(o5logon_verifier, 'base64'), E'\n', '')
    ))
)
WHERE o5logon_salt IS NOT NULL OR o5logon_verifier IS NOT NULL;

--bun:split

ALTER TABLE api_keys DROP COLUMN o5logon_salt;

--bun:split

ALTER TABLE api_keys DROP COLUMN o5logon_verifier;
