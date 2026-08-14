-- Keep the default audit listing cheap now that every session writes to
-- audit_log.
--
-- `connection.opened` / `connection.closed` are what make a whole-session
-- delete (DELETE FROM connections, which cascades through queries and
-- query_rows) leave evidence in a table the delete does not touch. The cost is
-- volume: a proxy serving ten thousand sessions a day writes twenty thousand of
-- these against a handful of control-plane changes, and audit_log is never
-- reaped by retention.
--
-- ListAuditEvents therefore leaves them out of an unfiltered listing. Without
-- this index that exclusion is a filter on top of a descending scan of the
-- primary key, so filling one 50-row page reads ~50 * (session events per
-- control-plane event) rows — bounded by the ratio, not by the size of the
-- table, but a few tens of thousands of heap fetches on a busy store. The
-- partial index holds only the rows the default listing can return, so the page
-- costs its own length again.
--
-- The predicate is written exactly as the query builds it, because a partial
-- index is only usable when the planner can prove the query's restriction
-- implies it.
CREATE INDEX idx_audit_log_control_plane_uid
    ON audit_log (uid DESC)
    WHERE event_type NOT IN ('connection.opened', 'connection.closed');
