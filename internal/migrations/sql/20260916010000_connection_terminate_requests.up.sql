-- An admin asking for one live session to end, across the instance boundary.
--
-- The replica that serves POST /connections/{uid}/terminate does not
-- necessarily own the session: connections.run_id says who does, and a run id
-- is minted in memory, so there is no in-process channel between the two. The
-- request is therefore written here and each process polls for its own.
--
-- terminate_reason is the requesting admin's free text, not the vocabulary
-- written to termination_reason when the session actually ends. The two are
-- deliberately different columns: this one is an intent that may never be acted
-- on (the session can close on its own first), that one is a record of what
-- happened.
ALTER TABLE connections
    ADD COLUMN IF NOT EXISTS terminate_requested_at timestamptz,
    ADD COLUMN IF NOT EXISTS terminate_requested_by uuid REFERENCES users(uid) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS terminate_reason text;

--bun:split

-- The poller runs every 2 seconds in every replica, forever, so its first arm
-- must touch almost nothing. Partial on both predicates: a requested
-- termination is rare and a *pending* one is rarer still — the row stops
-- matching the moment the session ends — so the index holds a handful of rows
-- at any instant whatever the size of the table.
CREATE INDEX IF NOT EXISTS idx_connections_terminate_requested
    ON connections (run_id)
    WHERE terminate_requested_at IS NOT NULL AND disconnected_at IS NULL;
