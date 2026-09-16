ALTER TABLE connections DROP COLUMN IF EXISTS termination_reason;

--bun:split

ALTER TABLE grant_definitions DROP COLUMN IF EXISTS statement_timeout_seconds;
