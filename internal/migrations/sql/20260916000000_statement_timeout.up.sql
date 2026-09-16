-- Per-statement time limit, and the record of what dbbat did when one was
-- crossed.
--
-- grant_definitions.statement_timeout_seconds is a three-state column, which is
-- the whole point of it being nullable:
--
--   NULL — inherit the instance-wide default (limits.statement_timeout, else
--          DBB_STATEMENT_TIMEOUT). The state every pre-existing definition
--          upgrades into, so nothing changes on migration alone.
--   0    — explicitly *no* limit for this definition, overriding the global
--          one. With a global limit set this is the only way to keep a dump or
--          an ETL definition usable through dbbat, and it is an admin decision
--          taken at definition-edit time — which is what "no bypass" protects:
--          the client can never widen its own limit, only an operator can.
--   > 0  — the limit, in seconds.
--
-- Definitions are immutably versioned, so an edit archives the row and inserts
-- a successor: a live grant keeps the value it was issued under, exactly like
-- duration_seconds and the quotas.
ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS statement_timeout_seconds bigint;

--bun:split

-- Why a session ended, when dbbat is the one that ended it. NULL — the only
-- value a pre-existing row can have, and the value of every ordinary close —
-- means the client (or the network) ended it.
--
-- Written in the same UPDATE as disconnected_at, so a terminated session can
-- never read as a clean one. The vocabulary is deliberately small and stable:
-- statement_timeout, grant_expired, quota_exceeded, grant_revoked, and later
-- admin_terminated.
ALTER TABLE connections
    ADD COLUMN IF NOT EXISTS termination_reason text;
