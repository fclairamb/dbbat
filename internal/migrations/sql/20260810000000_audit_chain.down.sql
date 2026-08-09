ALTER TABLE connections
    DROP COLUMN IF EXISTS query_chain_mac,
    DROP COLUMN IF EXISTS query_chain_len;

--bun:split

DROP INDEX IF EXISTS idx_queries_chain_seq;

--bun:split

ALTER TABLE queries
    DROP COLUMN IF EXISTS chain_seq,
    DROP COLUMN IF EXISTS prev_mac,
    DROP COLUMN IF EXISTS mac;

--bun:split

DELETE FROM audit_log WHERE uid = '00000000-0000-7000-8000-0000dbba7000'::uuid;

--bun:split

DROP INDEX IF EXISTS idx_audit_log_chain_seq;

--bun:split

ALTER TABLE audit_log
    DROP COLUMN IF EXISTS chain_seq,
    DROP COLUMN IF EXISTS prev_mac,
    DROP COLUMN IF EXISTS mac;
