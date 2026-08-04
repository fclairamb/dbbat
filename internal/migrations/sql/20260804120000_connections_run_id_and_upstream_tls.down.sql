-- Reverses the up-migration in the opposite order to the one it applied them.

ALTER TABLE connections DROP COLUMN IF EXISTS upstream_tls;

--bun:split

-- Collapse the registry back to one row per instance id, keeping the freshest
-- run of each. Required before the single-column primary key can come back:
-- the pair key is what allows several rows per id in the first place, so a
-- rollback with two live replicas sharing an id would otherwise fail on a
-- duplicate key.
DELETE FROM instances AS a
USING instances AS b
WHERE a.instance_id = b.instance_id
  AND (a.last_seen_at, a.run_id) < (b.last_seen_at, b.run_id);

--bun:split

ALTER TABLE instances DROP CONSTRAINT IF EXISTS instances_pkey;

--bun:split

ALTER TABLE instances DROP COLUMN IF EXISTS run_id;

--bun:split

ALTER TABLE instances ADD CONSTRAINT instances_pkey PRIMARY KEY (instance_id);

--bun:split

ALTER TABLE connections DROP COLUMN IF EXISTS run_id;
