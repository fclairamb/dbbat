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

-- Seed the registry from every instance id the connections table has ever
-- recorded, so the rolling upgrade onto this migration cannot reclaim a live
-- session.
--
-- No replica still running the previous build can register itself — that code
-- does not know this table exists — so at this instant every one of them looks
-- dead, and rows predating the instance_id column carry '' and have no owner at
-- all. Without a seed, the first process to start on the new code would see "no
-- instances row" for all of them and close connections that are still being
-- served. Seeding gives each a fresh last_seen_at: the full grace period to
-- prove it is alive by heartbeating, which in practice means being replaced by
-- a new-build pod before the rollout ends. Whatever does not (a dead pod, a
-- legacy '' row) goes stale and is reclaimed later, and the stale rows are
-- pruned by the next startup reconcile.
--
-- Seeding from the whole table rather than only the currently-open connections
-- is deliberate: a replica that happens to be idle at this instant owns no open
-- connection, would not be seeded, and could accept a session a moment later
-- with no registry row to protect it. Every replica that has served anything in
-- the retained history is covered this way. DISTINCT over connections is a
-- one-off scan of an already-indexed table, and the extra rows cost one prune.
INSERT INTO instances (instance_id, started_at, last_seen_at)
SELECT DISTINCT instance_id, now(), now()
FROM connections
ON CONFLICT (instance_id) DO NOTHING;
