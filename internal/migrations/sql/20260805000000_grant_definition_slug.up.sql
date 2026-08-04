-- Grant definitions are only addressable by uid (a UUID) today. Anything
-- outside the web UI — CLI, agents, scripted REST calls, runbooks — has to
-- list definitions and copy a UUID before it can reference one. slug gives
-- those callers a stable, human-typeable, machine-friendly handle.
--
-- Added nullable first so the column can exist on a populated table, then
-- backfilled, then locked down to NOT NULL + UNIQUE. Adding NOT NULL before
-- the backfill would fail outright on any existing row.
ALTER TABLE grant_definitions ADD COLUMN IF NOT EXISTS slug TEXT;

--bun:split

-- uid::text is trivially unique (it's a UUID) and requires no coordination
-- with whatever's already stored in `name`, which is free text and not
-- unique. Operators can rename the slug to something friendlier afterward;
-- this backfill only has to satisfy the NOT NULL + UNIQUE constraints below.
UPDATE grant_definitions SET slug = uid::text WHERE slug IS NULL;

--bun:split

ALTER TABLE grant_definitions ALTER COLUMN slug SET NOT NULL;

--bun:split

ALTER TABLE grant_definitions ADD CONSTRAINT grant_definitions_slug_uniq UNIQUE (slug);
