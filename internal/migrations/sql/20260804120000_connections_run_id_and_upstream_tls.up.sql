-- Two schema changes that ship together: per-run ownership of a connection,
-- and whether that connection's upstream leg was encrypted. They are separate
-- concerns, kept in one migration because they were written against the same
-- release and there is no version of dbbat that has one without the other.

-- Identifies the *run* that owns a connection, not just the process name.
--
-- instance_id (20260803020000) plus the instances registry (20260803030000)
-- answer "is this row's owner alive?" only as long as one instance id means one
-- live process. That assumption is a configuration away from being false: an
-- operator can set the same DBB_INSTANCE_ID on several replicas, and
-- config.FallbackInstanceID ("dbbat") is reached by every replica that cannot
-- read its hostname. When it breaks, a starting replica closes a live peer's
-- sessions — and those rows immediately satisfy the retention sweep's
-- disconnected_at cutoff, so the sweep can delete a connection a live session
-- is still writing queries against.
--
-- run_id is a UUID minted in memory at every start and never configurable, so
-- it is unique per running process by construction. Stamped on the connection
-- rows a run opens, and on that run's registry row, it lets the reconcile ask
-- "does a *live run* own this row?" instead of "is this my id?".
--
-- Nullable on purpose: NULL means "opened before run tracking existed", which
-- is a different statement from "opened by a run whose id happens to be
-- empty". Store.noLiveOwner falls back to the instance-id-wide liveness test
-- for those rows, which is exactly the rule their owner (a build that only
-- maintains the per-id registry) is playing by.
ALTER TABLE connections ADD COLUMN IF NOT EXISTS run_id TEXT;

--bun:split

-- The registry now describes a run rather than an id, so two live replicas
-- sharing one instance id are both represented instead of the second one's
-- upsert overwriting the first one's heartbeat — which would have made the
-- first look dead while it was serving traffic.
--
-- '' is the run id of every row written before this migration: the rows seeded
-- by 20260803030000, and any still-running process on a build that predates
-- run tracking.
ALTER TABLE instances ADD COLUMN IF NOT EXISTS run_id TEXT NOT NULL DEFAULT '';

--bun:split

-- Drop-then-add rather than a bare ADD, so re-running this is not an error.
ALTER TABLE instances DROP CONSTRAINT IF EXISTS instances_pkey;

--bun:split

ALTER TABLE instances ADD CONSTRAINT instances_pkey PRIMARY KEY (instance_id, run_id);

--bun:split

-- Buy every pre-run-tracking process a full grace period, the same way
-- 20260803030000 seeded the registry for the upgrade that introduced it.
--
-- A process on a build that predates this migration refreshes its row with
-- "ON CONFLICT (instance_id) DO UPDATE", and that conflict target no longer
-- exists once the primary key is the pair — so from this instant its
-- heartbeats fail and its row stops moving. A rolling upgrade normally
-- replaces such a process in far less than the 15-minute grace period, and
-- this refresh makes sure the clock starts now rather than at whatever
-- last_seen_at it managed to write before the migration ran.
UPDATE instances SET last_seen_at = now() WHERE run_id = '';

--bun:split

-- Records whether the proxy→upstream leg of a session was actually encrypted.
--
-- ssl_mode on the server row states a *policy*, not an outcome. Under the
-- opportunistic modes ("prefer", and the empty default that every unset row
-- carries) dbbat offers TLS and falls back to plaintext when the target refuses
-- — which is exactly what makes those modes usable, and exactly what makes them
-- impossible to audit from configuration alone. Two sessions against the same
-- row, minutes apart, can differ if the target's TLS was reloaded in between.
--
-- Without this column "are we actually encrypted?" is a guess. With it, the
-- fallback is safe to keep: a downgrade is recorded rather than silent, and an
-- operator can find the rows where it happened instead of inferring it.
--
-- NOT NULL DEFAULT false: rows written before this migration were opened by a
-- build with no opportunistic TLS on MySQL or MongoDB, and by a PostgreSQL path
-- that did not report what it negotiated. "false" is the honest reading of a
-- row that never measured it — deliberately not NULL, because a nullable
-- boolean here would invite "unknown" to be treated as "encrypted".
ALTER TABLE connections ADD COLUMN IF NOT EXISTS upstream_tls BOOLEAN NOT NULL DEFAULT false;
