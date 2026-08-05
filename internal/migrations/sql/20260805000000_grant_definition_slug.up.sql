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

-- A UUID is not a valid slug (the API rejects one at request time — it would
-- defeat the whole point of a human-typeable handle), so the backfill can't
-- just copy uid::text wholesale the way an earlier version of this migration
-- did. Instead: 'legacy-' + the uid's first hex group + a row number.
--
--   * 'legacy-' || left(gd.uid::text, 8): Postgres always renders a uuid's
--     ::text cast in the canonical lowercase 8-4-4-4-12 hyphenated layout,
--     so left(..., 8) is guaranteed to be exactly the first group with no
--     hyphen in it — this DOES rely on that canonical layout, called out
--     here rather than left implicit. It keeps the backfilled slug
--     traceable back to the row's uid without being anywhere near
--     UUID-shaped itself.
--   * The 'legacy-' prefix contains letters outside a-f (l, g, y), so the
--     result can never be interpreted as a hex string under either UUID
--     form uuid.Parse accepts (36-char hyphenated or bare 32-hex-digit) —
--     that's true independent of length, not just because it's short.
--   * Uniqueness is NOT coming from the uid fragment: these are UUIDv7,
--     whose leading hex encodes a millisecond timestamp, so two definitions
--     created in the same millisecond would share their first 8 hex chars.
--     The actual uniqueness guarantee is the row_number() suffix below,
--     which is strictly distinct per row by construction regardless of any
--     collision in the uid truncation.
--   * The result matches slugPattern (^[a-z0-9]+(-[a-z0-9]+)*$): three
--     non-empty lowercase-alphanumeric segments joined by single hyphens,
--     never a leading/trailing/doubled hyphen.
--
-- Operators are expected to rename these — the 'legacy-' prefix makes that
-- obvious rather than leaving a slug that looks like a deliberate choice.
WITH legacy AS (
    SELECT uid, row_number() OVER (ORDER BY uid) AS rn
    FROM grant_definitions
    WHERE slug IS NULL
)
UPDATE grant_definitions AS gd
SET slug = 'legacy-' || left(gd.uid::text, 8) || '-' || legacy.rn::text
FROM legacy
WHERE gd.uid = legacy.uid;

--bun:split

ALTER TABLE grant_definitions ALTER COLUMN slug SET NOT NULL;

--bun:split

ALTER TABLE grant_definitions ADD CONSTRAINT grant_definitions_slug_uniq UNIQUE (slug);
