ALTER TABLE users DROP COLUMN IF EXISTS last_login_at;

--bun:split

DROP INDEX IF EXISTS idx_connections_user_connected_at;

--bun:split

DROP INDEX IF EXISTS idx_connections_grant_uid;

--bun:split

DROP INDEX IF EXISTS idx_grant_requests_resulting_grant;
