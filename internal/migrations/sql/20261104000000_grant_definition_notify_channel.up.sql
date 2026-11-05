-- Which Slack channel a grant definition's notifications go to.
--
--   NULL   — the instance default channel (DBB_SLACK_NOTIFY_CHANNEL). Every
--            pre-existing definition upgrades into this state.
--   'none' — post nothing for this definition.
--   other  — a key of DBB_SLACK_NOTIFY_CHANNELS.
ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS notify_channel text;

--bun:split

-- An auto-approved renewal is posted as a reply in the thread of the first
-- request of its chain. The renewal's slack_channel / slack_message_ts then
-- hold the ROOT's coordinates, and this column names the root so the notifier
-- never chat.update's a message that is not the request's own.
ALTER TABLE grant_requests
    ADD COLUMN IF NOT EXISTS slack_thread_root_uid uuid
        REFERENCES grant_requests(uid) ON DELETE SET NULL;
