-- Stamps each connection with the grant it authenticated under.
--
-- Until now the auth-time GetActiveGrant pick was recorded nowhere: the UI
-- could not show "under which grant did this session run?", mayApproveQuery
-- had to re-resolve the active grant at approval time (picking a *newer*
-- overlapping grant's approver groups if one was created while a query sat on
-- hold), and quota counters could only aggregate by a (user, database) time
-- window, so overlapping grants each got charged for the same traffic.
--
-- Nullable: pre-existing rows predate the stamp and stay NULL forever — there
-- is no way to retroactively know which grant an already-closed session ran
-- under. No ON DELETE clause on the FK: access_grants rows are revoked, never
-- deleted, so there is nothing to cascade.
ALTER TABLE connections ADD COLUMN IF NOT EXISTS grant_uid uuid REFERENCES access_grants(uid);

--bun:split

-- No index: connections are looked up by uid (PK) or by (user_id,
-- database_id) via the existing composite work; grant_uid is read back for a
-- single row at a time (mayApproveQuery, the connection detail endpoint) or
-- alongside a scan already keyed elsewhere (populateGrantCounters joins
-- queries -> connections on connection_id, and gains grant_uid for free off
-- that same row).
COMMENT ON COLUMN connections.grant_uid IS
    'The access grant this session authenticated under (the auth-time GetActiveGrant pick), pinned for the life of the connection and never updated afterwards. NULL on rows written before this column existed, or if the owning grant has since been deleted.';
