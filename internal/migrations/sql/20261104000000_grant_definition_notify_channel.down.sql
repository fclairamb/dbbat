ALTER TABLE grant_requests DROP COLUMN IF EXISTS slack_thread_root_uid;

--bun:split

ALTER TABLE grant_definitions DROP COLUMN IF EXISTS notify_channel;
