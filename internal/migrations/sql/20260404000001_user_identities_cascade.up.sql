ALTER TABLE user_identities DROP CONSTRAINT IF EXISTS user_identities_user_id_fkey;

--bun:split

ALTER TABLE user_identities ADD CONSTRAINT user_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(uid) ON DELETE CASCADE;
