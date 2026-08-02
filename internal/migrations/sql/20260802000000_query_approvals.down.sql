ALTER TABLE access_grants
    DROP COLUMN approval_patterns,
    DROP COLUMN approver_group_uids;

--bun:split

ALTER TABLE grant_definitions
    DROP COLUMN approval_patterns,
    DROP COLUMN approver_group_uids;

--bun:split

DROP INDEX IF EXISTS queries_pending_approval_idx;

--bun:split

ALTER TABLE queries
    DROP CONSTRAINT IF EXISTS queries_approval_status_check;

--bun:split

ALTER TABLE queries
    DROP COLUMN approval_status,
    DROP COLUMN approval_pattern,
    DROP COLUMN resolved_by,
    DROP COLUMN resolved_at,
    DROP COLUMN resolution_reason;
