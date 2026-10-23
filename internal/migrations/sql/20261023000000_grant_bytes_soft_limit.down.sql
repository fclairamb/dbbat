ALTER TABLE access_grants DROP COLUMN IF EXISTS bytes_notified_at;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS bytes_notified_steps;

--bun:split

ALTER TABLE grant_definitions DROP COLUMN IF EXISTS notify_bytes_step;
