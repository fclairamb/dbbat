-- Which Slack channel a grant definition's notifications go to.
--
--   NULL   — the instance default channel (DBB_SLACK_NOTIFY_CHANNEL). Every
--            pre-existing definition upgrades into this state.
--   'none' — post nothing for this definition.
--   other  — a key of DBB_SLACK_NOTIFY_CHANNELS.
ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS notify_channel text;
