-- Consolidate protocol-specific API-key material into a single jsonb column
-- (protocol_data) instead of dedicated per-protocol columns. It holds the Oracle
-- O5LOGON verifiers (6949 + 18453) under an "oracle" key.
--
-- All existing verifier bytes are migrated in, so no key needs regenerating. The
-- verifier-6949 columns are always present (added on main); the verifier-18453
-- columns only exist on databases that ran an earlier, now-removed migration, so
-- they are folded in conditionally. Every database converges on the same jsonb
-- layout regardless of which path it took.
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

-- Fold the verifier-18453 columns into the same protocol_data.oracle object where
-- they exist (databases that ran the removed 20260620 migration), then drop them.
-- Guarded by a column-existence check so this is a no-op on databases that never
-- had the columns (fresh installs and the main upgrade path). The bytes are merged
-- into the oracle object built above, preserving the verifier-6949 fields.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'api_keys'
          AND column_name = 'o5logon_verifier_18453'
    ) THEN
        UPDATE api_keys
        SET protocol_data =
            coalesce(protocol_data, '{}'::jsonb) ||
            jsonb_build_object('oracle',
                coalesce(protocol_data -> 'oracle', '{}'::jsonb) || jsonb_strip_nulls(jsonb_build_object(
                    'o5logon_salt_18453',     replace(encode(o5logon_salt_18453, 'base64'), E'\n', ''),
                    'o5logon_verifier_18453', replace(encode(o5logon_verifier_18453, 'base64'), E'\n', '')
                ))
            )
        WHERE o5logon_salt_18453 IS NOT NULL OR o5logon_verifier_18453 IS NOT NULL;

        ALTER TABLE api_keys DROP COLUMN o5logon_salt_18453;
        ALTER TABLE api_keys DROP COLUMN o5logon_verifier_18453;
    END IF;
END $$;

--bun:split

ALTER TABLE api_keys DROP COLUMN o5logon_salt;

--bun:split

ALTER TABLE api_keys DROP COLUMN o5logon_verifier;
