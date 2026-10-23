-- Soft limit on the bytes a grant transfers: a Slack notice each time the
-- grant's cumulative bytes cross another multiple of a step. Nothing is ever
-- blocked.
--
-- grant_definitions.notify_bytes_step has the same three states as
-- statement_timeout_seconds:
--
--   NULL — inherit the instance-wide DBB_SLACK_NOTIFY_BYTES_STEP. Every
--          pre-existing definition upgrades into this state.
--   0    — no notice for this definition, overriding the instance default
--          (a dump definition opts out).
--   > 0  — the step, in bytes.
ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS notify_bytes_step bigint;

--bun:split

-- How many steps have already been announced for this grant, and when the
-- last one was. The counter is what dedupes across restarts and replicas: a
-- notice is only posted by the replica whose conditional UPDATE moved it.
ALTER TABLE access_grants
    ADD COLUMN IF NOT EXISTS bytes_notified_steps integer NOT NULL DEFAULT 0;

--bun:split

ALTER TABLE access_grants
    ADD COLUMN IF NOT EXISTS bytes_notified_at timestamptz;
