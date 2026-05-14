ALTER TABLE users DROP CONSTRAINT IF EXISTS users_username_key;

--bun:split

CREATE UNIQUE INDEX users_username_active_uq
    ON users(username)
    WHERE deleted_at IS NULL;
