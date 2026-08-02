-- Approval holds: a query matching one of the grant's approval patterns is
-- persisted immediately with approval_status = 'pending' and parked until a
-- second human resolves it. There is deliberately no 'timeout' state: a hold
-- ends on approve, deny, or client disconnect ('abandoned') only.
ALTER TABLE queries
    ADD COLUMN approval_status   text,
    ADD COLUMN approval_pattern  text,
    ADD COLUMN resolved_by       uuid,
    ADD COLUMN resolved_at       timestamptz,
    ADD COLUMN resolution_reason text;

--bun:split

ALTER TABLE queries
    ADD CONSTRAINT queries_approval_status_check
    CHECK (approval_status IS NULL OR approval_status IN ('pending', 'approved', 'denied', 'abandoned'));

--bun:split

-- Partial index: the pending set is tiny relative to the queries table, and it
-- is read on every "what is pending now" refresh and on every reconnect.
CREATE INDEX queries_pending_approval_idx ON queries (uid) WHERE approval_status = 'pending';

--bun:split

-- Patterns live on the grant definition and are mirrored onto the materialized
-- grant (exactly like controls / quotas), so the proxy session never has to
-- join back to the definition on the hot path — and so admin-created grants,
-- which bypass definitions entirely, can carry patterns too.
ALTER TABLE grant_definitions
    ADD COLUMN approval_patterns   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN approver_group_uids uuid[] NOT NULL DEFAULT '{}';

--bun:split

ALTER TABLE access_grants
    ADD COLUMN approval_patterns   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN approver_group_uids uuid[] NOT NULL DEFAULT '{}';
