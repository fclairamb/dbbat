-- Tamper-evident audit chain.
--
-- Every audit_log row written from here on carries an HMAC over its own
-- content plus the previous row's HMAC, so altering, deleting or reordering an
-- entry breaks the chain. The key is derived from DBB_KEY with HKDF and never
-- reaches this database: whoever can write these tables still cannot forge the
-- MACs. Verification is `dbbat audit verify`.
--
-- chain_seq is the chain's own ordering. It is not uid: UUIDv7 only orders by
-- millisecond, so two rows minted in the same millisecond have no defined
-- order, and a chain has to have exactly one.
ALTER TABLE audit_log
    ADD COLUMN chain_seq BIGINT,
    ADD COLUMN prev_mac BYTEA,
    ADD COLUMN mac BYTEA;

-- Partial, because every row written before this migration keeps a NULL
-- chain_seq: those rows predate chaining and are documented as unverifiable.
CREATE UNIQUE INDEX idx_audit_log_chain_seq ON audit_log(chain_seq) WHERE chain_seq IS NOT NULL;

--bun:split

-- The anchor marks where chaining begins. It is chain_seq 0 and carries no MAC
-- of its own: it is a marker, not a link. The first chained row is chain_seq 1
-- and its prev_mac is a genesis MAC derived from the key, so deleting the
-- earliest real entries is still detected.
--
-- The uid is a fixed constant so verification can find the anchor without
-- guessing, and ON CONFLICT makes re-running the migration on a database that
-- already has it a no-op.
INSERT INTO audit_log (uid, event_type, details, created_at, chain_seq)
VALUES (
    '00000000-0000-7000-8000-0000dbba7000'::uuid,
    'audit.chain_anchor',
    jsonb_build_object(
        'note', 'audit log HMAC chaining starts here; earlier rows are unverifiable'
    ),
    NOW(),
    0
)
ON CONFLICT (uid) DO NOTHING;

--bun:split

-- The query chain is per connection, not global: DBB_QUERY_STORAGE_RETENTION
-- deletes whole connections, which would sever a single global chain on its
-- first sweep. One chain per connection means a reaped connection takes its
-- own chain with it and leaves every other connection verifiable end to end.
ALTER TABLE queries
    ADD COLUMN chain_seq BIGINT,
    ADD COLUMN prev_mac BYTEA,
    ADD COLUMN mac BYTEA;

CREATE UNIQUE INDEX idx_queries_chain_seq ON queries(connection_id, chain_seq) WHERE chain_seq IS NOT NULL;

--bun:split

-- The final chain head, stamped when the session closes. Without it, deleting
-- the *last* queries of a connection would leave a chain that still verifies;
-- with it, the stored head no longer matches what the surviving rows compute.
ALTER TABLE connections
    ADD COLUMN query_chain_mac BYTEA,
    ADD COLUMN query_chain_len BIGINT NOT NULL DEFAULT 0;
