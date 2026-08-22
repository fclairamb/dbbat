-- Indexes for the connection/query observability filters, plus the users
-- last-login column.
--
-- grant_requests(resulting_grant_id): the grant-provenance filter walks the
-- request → grant link *backwards* ("which request produced the grant this
-- session ran under?"), and 20260510000000 indexed only (user_id, status) and
-- (status, requested_at). Without this index every provenance-filtered page is
-- a sequential scan of grant_requests per candidate connection. Partial: a
-- request that never produced a grant (pending, denied, cancelled, expired)
-- can never satisfy the lookup, and those are the majority of rows on a busy
-- instance.
CREATE INDEX IF NOT EXISTS idx_grant_requests_resulting_grant
    ON grant_requests (resulting_grant_id)
    WHERE resulting_grant_id IS NOT NULL;

--bun:split

-- connections(grant_uid): 20260806010000 deliberately shipped the column
-- without an index because nothing filtered on it. The grant_uid /
-- grant_definition_uid filters do, so it is needed now. Partial for the same
-- reason as above — every session predating the stamp, and every session that
-- ran without a grant, is NULL and matches no grant filter.
CREATE INDEX IF NOT EXISTS idx_connections_grant_uid
    ON connections (grant_uid)
    WHERE grant_uid IS NOT NULL;

--bun:split

-- connections(user_id, connected_at DESC): supports the per-user
-- last-connection read (DISTINCT ON (user_id) ... ORDER BY user_id,
-- connected_at DESC), which the plain idx_connections_user_id from the initial
-- schema can only serve by sorting every one of a user's sessions.
CREATE INDEX IF NOT EXISTS idx_connections_user_connected_at
    ON connections (user_id, connected_at DESC);

--bun:split

-- users.last_login_at: when this account last signed in *interactively* —
-- password login or an OAuth/OIDC login. Deliberately NOT touched by JWT
-- validation, session refresh, API-key authentication or MCP: the moment a
-- background token refresh moves it, the column stops meaning "last seen in
-- the UI" and starts meaning "last HTTP request from anything", which answers
-- no question anybody asked.
--
-- Operational state, not evidence: it is a mutable column outside the audit
-- HMAC chain, so it is not tamper-evident. An evidential login history would
-- be chained `user.login` audit events, which is a different feature.
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_login_at timestamptz;

--bun:split

COMMENT ON COLUMN users.last_login_at IS
    'Last interactive sign-in (password login or OAuth/OIDC), NULL if never. Not written by token validation, session refresh, API-key auth or MCP. Mutable operational state, outside the audit chain.';
