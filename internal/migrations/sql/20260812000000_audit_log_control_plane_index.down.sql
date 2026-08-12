-- Dropping the index only makes the default audit listing slower; the entries
-- it indexes stay, chained, wherever they were written.
DROP INDEX IF EXISTS idx_audit_log_control_plane_uid;
