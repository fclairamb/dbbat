-- Dropping the version does not un-seal the stamps already written: rows
-- closed by a version 1 build keep a keyed query_chain_mac that the older
-- verifier will compare against a raw head MAC and report as a break. Rolling
-- back is therefore a rollback of the binary too.
ALTER TABLE connections
    DROP COLUMN IF EXISTS query_chain_stamp_version;
