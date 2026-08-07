-- Grants get an explicit priority so multi-grant selection is deliberate.
--
-- Until now GetActiveGrant picked the newest-created active grant for a
-- (user, database) pair. Overlap is trivially reachable — nothing rejects a
-- second active grant at insert time — so a user holding both a read-only and
-- a read-write grant on the same database got whichever they happened to
-- create last, and every downstream control (read-only pinning, quotas,
-- approval patterns) followed that accidental pick.
--
-- Correction of record: the initial schema
-- (20260107000000_initial_schema.up.sql, next to idx_access_grants_active)
-- claims "Active grant uniqueness is enforced at application level". That was
-- never true — no code path has ever rejected an overlapping active grant, and
-- this change deliberately embraces overlap instead of forbidding it. The
-- claim is corrected here rather than edited there because shipped migrations
-- are immutable.
ALTER TABLE access_grants ADD COLUMN IF NOT EXISTS priority SMALLINT NOT NULL DEFAULT 0;

--bun:split

-- Backfill the auto-calculated tier for every pre-existing row, mirroring
-- store.AutoPriority exactly:
--
--   read_only (with or without other controls) -> 10
--   writable with controls (block_copy/block_ddl) -> 50
--   writable, no controls (full R/W)             -> 100
--
-- The gaps between tiers exist so an operator can slot a manual override
-- between two tiers without renumbering anything.
UPDATE access_grants
SET priority = CASE
    WHEN 'read_only' = ANY(controls) THEN 10
    WHEN array_length(controls, 1) IS NULL THEN 100
    ELSE 50
END;

--bun:split

-- Grant definitions carry an *optional* priority: NULL means "compute it from
-- the controls when a grant is materialized", which is what every existing
-- definition wants. A non-NULL value is copied verbatim onto the grant, so an
-- operator can pin a definition below or above its natural tier.
ALTER TABLE grant_definitions ADD COLUMN IF NOT EXISTS priority SMALLINT;

--bun:split

-- No index: idx_access_grants_active already narrows the candidate set to a
-- handful of rows per (user, database), and sorting three columns over that is
-- free.
COMMENT ON COLUMN access_grants.priority IS
    'Selection rank among overlapping active grants; highest wins. Auto-calculated from controls (100 full R/W, 50 writable-with-controls, 10 read-only) unless explicitly set.';
