-- The query-history retention sweep (DBB_QUERY_STORAGE_RETENTION) reaps
-- connections closed before its cutoff. Without this index that predicate is a
-- sequential scan over every connection ever made, on every sweep. Partial:
-- connections still open are never candidates, so they stay out of the index.
CREATE INDEX IF NOT EXISTS idx_connections_disconnected_at
    ON connections (disconnected_at)
    WHERE disconnected_at IS NOT NULL;
