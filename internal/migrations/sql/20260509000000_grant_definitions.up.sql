CREATE TABLE grant_definitions (
    uid                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    duration_seconds      BIGINT NOT NULL CHECK (duration_seconds > 0),
    controls              TEXT[] NOT NULL DEFAULT '{}',
    max_query_counts      BIGINT,
    max_bytes_transferred BIGINT,
    is_active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_by            UUID NOT NULL REFERENCES users(uid),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX grant_definitions_active_name_uniq
    ON grant_definitions(name) WHERE is_active;
