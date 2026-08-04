-- Reverses the up-migration in the opposite order to the one it applied them.

ALTER TABLE grant_definitions DROP CONSTRAINT IF EXISTS grant_definitions_slug_uniq;

--bun:split

ALTER TABLE grant_definitions DROP COLUMN IF EXISTS slug;
