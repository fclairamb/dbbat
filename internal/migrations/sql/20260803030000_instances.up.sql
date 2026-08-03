-- Liveness registry for the dbbat processes sharing this store.
--
-- Connection rows carry the instance_id of the process that opened them
-- (20260803020000_connections_instance_id), which lets a restarting process
-- reconcile its own crash orphans without touching another replica's live
-- sessions. Identity alone cannot tell "gone" from "busy", though, so rows
-- owned by an id that never comes back — a plain Kubernetes Deployment mints a
-- new pod name on every restart — were never reclaimed and accumulated forever.
--
-- Each process now upserts its row here at startup, refreshes last_seen_at on a
-- short ticker, and deletes the row on a clean shutdown. The startup reconcile
-- can then replace "is this row mine?" with "is this row's owner alive?": a
-- connection whose instance_id has no row here, or whose row has not been seen
-- for the (generous) stale-instance grace period, belongs to a process that is
-- provably gone.
CREATE TABLE IF NOT EXISTS instances (
    instance_id  TEXT PRIMARY KEY,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

--bun:split

-- Seed the registry from whoever currently owns an open connection, so the
-- rolling upgrade onto this migration cannot reclaim a live session.
--
-- The instances that own the open connections at this instant have not had a
-- chance to register themselves yet — the replicas still running the previous
-- build never will, and rows predating the instance_id column carry '' and have
-- no owner at all. Without this seed the first process to start on the new code
-- would see "no instances row" for every one of them and close connections that
-- are still being served. Seeding gives them all a fresh last_seen_at, i.e. the
-- full grace period to prove they are alive by heartbeating. Whatever does not
-- (a dead pod, a legacy '' row) simply goes stale and is reclaimed later.
INSERT INTO instances (instance_id, started_at, last_seen_at)
SELECT DISTINCT instance_id, now(), now()
FROM connections
WHERE disconnected_at IS NULL
ON CONFLICT (instance_id) DO NOTHING;
