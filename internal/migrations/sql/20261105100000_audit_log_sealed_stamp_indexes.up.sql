-- Indexes behind the sealed-stamp cross-check of `dbbat audit verify`.
--
-- A chain stamp (connections.query_chain_mac, queries.row_chain_mac) lives on a
-- plain row, so clearing it together with the records it protects used to read
-- as "nothing was ever recorded". Verification now compares every stamp with
-- its sealed copy in audit_log — the session's connection.closed entry, or the
-- query.rows_sealed entry naming the capture — and looks that copy up per
-- connection and per query. Without these, each lookup scans every session
-- entry ever written. See internal/store/chain_sealed.go.
CREATE INDEX IF NOT EXISTS idx_audit_log_closed_connection_uid
    ON audit_log ((details->>'connection_uid'))
    WHERE event_type = 'connection.closed';

--bun:split

CREATE INDEX IF NOT EXISTS idx_audit_log_rows_sealed_details
    ON audit_log USING gin (details jsonb_path_ops)
    WHERE event_type = 'query.rows_sealed';

--bun:split

-- The default listing leaves out every session-volume event
-- (store.SessionAuditEventTypes), which has grown since this partial index was
-- written. A narrower predicate is still provably implied by the listing's, so
-- the old index stayed usable; it simply indexed rows the listing can never
-- return. Rebuilt with the full list.
DROP INDEX IF EXISTS idx_audit_log_control_plane_uid;

--bun:split

CREATE INDEX idx_audit_log_control_plane_uid
    ON audit_log (uid DESC)
    WHERE event_type NOT IN ('connection.opened', 'connection.closed', 'connection.terminated', 'query.rows_sealed');
