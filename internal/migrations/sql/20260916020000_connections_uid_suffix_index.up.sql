-- Expression index backing `GET /api/v1/connections?uid_suffix=<12 hex>`
-- (store.ConnectionFilter.UIDSuffix): `WHERE right(uid::text, 12) = $1`.
--
-- uid is a uuid column, not text, so an equality filter on its trailing
-- characters has to compare on the text form — there is no native way to
-- slice a uuid. right(uid::text, 12) is exactly the "c=" tag
-- shared.BuildUpstreamName stamps on the upstream application/program name:
-- the last 12 hex characters of a UUIDv7 are its purely-random tail, unlike
-- the leading, millisecond-granularity timestamp every connection opened in
-- the same instant shares. Without this index the filter would sequential
-- scan the whole table, computing right(uid::text, 12) per row.
CREATE INDEX IF NOT EXISTS idx_connections_uid_suffix
    ON connections (right(uid::text, 12));
