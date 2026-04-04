CREATE TABLE user_identities (
    uid           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    provider      TEXT        NOT NULL,
    provider_id   TEXT        NOT NULL,
    email         TEXT,
    display_name  TEXT,
    metadata      JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at    TIMESTAMPTZ
);

--bun:split

CREATE UNIQUE INDEX idx_user_identities_provider_id
    ON user_identities (provider, provider_id)
    WHERE deleted_at IS NULL;

--bun:split

CREATE INDEX idx_user_identities_user_id ON user_identities (user_id) WHERE deleted_at IS NULL;

--bun:split

CREATE TABLE oauth_states (
    uid        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    state      TEXT        NOT NULL UNIQUE,
    provider   TEXT        NOT NULL,
    redirect_url TEXT,
    metadata   JSONB,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

--bun:split

CREATE INDEX idx_oauth_states_expires_at ON oauth_states (expires_at);
