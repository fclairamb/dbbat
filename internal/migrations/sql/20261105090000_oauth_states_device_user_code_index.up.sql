-- The device-authorization flow looks a request up by its user code
-- (deviceAuthUserCodeExists on every new request, then the approval page) with
-- `provider = 'device' AND metadata->>'user_code' = ?`. Without this index both
-- scan every oauth_states row.
CREATE INDEX IF NOT EXISTS idx_oauth_states_provider_user_code
    ON oauth_states (provider, (metadata->>'user_code'));
