CREATE TABLE grant_requests (
    uid                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID NOT NULL REFERENCES users(uid),
    grant_definition_id  UUID NOT NULL REFERENCES grant_definitions(uid),
    database_id          UUID NOT NULL REFERENCES databases(uid),
    justification        TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL CHECK (status IN
                          ('pending','approved','denied','cancelled','expired')),
    requested_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at           TIMESTAMPTZ,
    decided_by           UUID REFERENCES users(uid),
    decision_reason      TEXT,
    resulting_grant_id   UUID REFERENCES access_grants(uid),

    -- Slack notification bookkeeping (populated by Spec 04, NULL until then)
    slack_channel        TEXT,
    slack_message_ts     TEXT
);

CREATE INDEX grant_requests_user_status_idx
    ON grant_requests(user_id, status);

CREATE INDEX grant_requests_status_requested_idx
    ON grant_requests(status, requested_at);
