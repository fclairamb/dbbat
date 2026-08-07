-- Reverses the up-migration. Dropping the columns loses any manual override an
-- operator set; the auto-calculated values are recomputed on re-apply.
ALTER TABLE grant_definitions DROP COLUMN IF EXISTS priority;

--bun:split

ALTER TABLE access_grants DROP COLUMN IF EXISTS priority;
