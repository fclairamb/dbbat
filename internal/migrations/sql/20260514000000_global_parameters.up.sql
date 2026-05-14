CREATE TABLE global_parameters (
    uid       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_key TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

--bun:split

CREATE INDEX idx_global_parameters_group_key ON global_parameters(group_key);
CREATE UNIQUE INDEX idx_global_parameters_unique
    ON global_parameters(group_key, key) WHERE deleted_at IS NULL;
CREATE INDEX idx_global_parameters_deleted_at
    ON global_parameters(deleted_at) WHERE deleted_at IS NULL;
