-- Modern verifier-18453 (12c PBKDF2/HMAC-SHA512) O5LOGON material, stored
-- alongside the legacy verifier-6949 so the Oracle proxy can issue challenges
-- that modern thin clients (python-oracledb thin, JDBC thin / SQLcl, sqlplus
-- against Oracle 12c+/23ai) accept. Existing keys lack it until regenerated.
ALTER TABLE api_keys ADD COLUMN o5logon_salt_18453 BYTEA;

--bun:split

ALTER TABLE api_keys ADD COLUMN o5logon_verifier_18453 BYTEA;
