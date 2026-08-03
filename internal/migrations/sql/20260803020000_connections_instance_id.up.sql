-- Records which dbbat process owns a connection row.
--
-- A crash, a kill -9 or a pod reschedule never runs CloseConnection, so
-- disconnected_at stays NULL forever. The query-history retention sweep only
-- reaps connections with disconnected_at IS NOT NULL (deleting a row a live
-- session still logs against would break the foreign key), so those orphans
-- survive every sweep. Store.CloseOrphanedConnections reconciles them at
-- startup.
--
-- That reconcile MUST be scoped to the starting process. dbbat runs with more
-- than one replica against a shared store (see docs/approvals.md, "Multiple
-- replicas", and charts/dbbat/values.yaml replicaCount), so a blanket
-- "UPDATE connections SET disconnected_at = last_activity_at WHERE
-- disconnected_at IS NULL" would let a starting replica close another
-- replica's *live* connections — and those rows would then satisfy the
-- retention sweep's predicate while a live session is still writing queries
-- against them.
--
-- Rows that predate this column get ''. Their owner is unknown, so they are
-- deliberately left unreclaimable rather than attributed to whichever replica
-- happens to restart first.
ALTER TABLE connections ADD COLUMN IF NOT EXISTS instance_id TEXT NOT NULL DEFAULT '';

--bun:split

-- Partial: the reconcile only ever looks at still-open connections, which are a
-- tiny fraction of the table.
CREATE INDEX IF NOT EXISTS idx_connections_instance_id_open
    ON connections (instance_id)
    WHERE disconnected_at IS NULL;
