-- DBBat Database Schema - Rollback Initial Migration

-- Drop tables in reverse order of creation (respecting foreign key dependencies)
DROP TABLE IF EXISTS audit_log;

--bun:split

DROP TABLE IF EXISTS access_grants;

--bun:split

DROP TABLE IF EXISTS query_rows;

--bun:split

DROP TABLE IF EXISTS queries;

--bun:split

DROP TABLE IF EXISTS connections;

--bun:split

DROP TABLE IF EXISTS databases;

--bun:split

DROP TABLE IF EXISTS api_keys;

--bun:split

DROP TABLE IF EXISTS users;

--bun:split

DROP TYPE IF EXISTS user_role;
