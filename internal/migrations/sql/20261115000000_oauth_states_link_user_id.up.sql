-- A signed-in user can attach an SSO identity to their own account: the state
-- row of that flow carries the user it is bound to, and the callback links the
-- returned identity onto that user instead of resolving it through the login
-- path. NULL for every ordinary login, device and exchange row.
ALTER TABLE oauth_states
    ADD COLUMN IF NOT EXISTS link_user_id UUID REFERENCES users(uid) ON DELETE CASCADE;
