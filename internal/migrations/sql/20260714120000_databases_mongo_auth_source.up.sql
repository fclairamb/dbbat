-- Per-database upstream MongoDB authSource. The MongoDB proxy authenticates to
-- the upstream with SCRAM-SHA-256, and the SCRAM authSource is the database in
-- which the proxy user's credentials live. Historically fixed to "admin" (the
-- MongoDB default, e.g. MONGO_INITDB_ROOT_USERNAME); this nullable column lets
-- targets whose proxy user lives in a different auth database configure it.
-- NULL keeps the "admin" default. Mirrors the nullable oracle_service_name
-- protocol-specific column.
ALTER TABLE databases ADD COLUMN mongo_auth_source text;
