-- Sample queries are representative SQL statements an author saves alongside
-- a definition's approval_patterns, so the patterns can be validated against
-- them instead of shipping an untested regex into an access-control path
-- (see POST /grant-definitions/validate-patterns). It is part of the
-- definition's matching config, not a separate bench entity, so it versions
-- for free with every edit exactly like approval_patterns does.
ALTER TABLE grant_definitions
    ADD COLUMN IF NOT EXISTS sample_queries TEXT[] NOT NULL DEFAULT '{}';

--bun:split

COMMENT ON COLUMN grant_definitions.sample_queries IS
    'Representative SQL statements used to validate approval_patterns at authoring time. Test fixtures, not first-class matchers — RE2 matching against approval_patterns is unchanged.';
