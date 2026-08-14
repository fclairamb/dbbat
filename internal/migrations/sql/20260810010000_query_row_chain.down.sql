ALTER TABLE queries
    DROP COLUMN IF EXISTS row_chain_mac,
    DROP COLUMN IF EXISTS row_chain_len;

--bun:split

ALTER TABLE query_rows
    DROP COLUMN IF EXISTS prev_mac,
    DROP COLUMN IF EXISTS mac;
