DROP INDEX IF EXISTS idx_audit_log_control_plane_uid;

--bun:split

CREATE INDEX idx_audit_log_control_plane_uid
    ON audit_log (uid DESC)
    WHERE event_type NOT IN ('connection.opened', 'connection.closed');

--bun:split

DROP INDEX IF EXISTS idx_audit_log_rows_sealed_details;

--bun:split

DROP INDEX IF EXISTS idx_audit_log_closed_connection_uid;
