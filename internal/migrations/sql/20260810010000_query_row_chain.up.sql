-- Tamper-evident chaining for the captured result rows.
--
-- 20260810000000_audit_chain sealed audit_log (one chain for the store) and
-- queries (one chain per connection). It left query_rows — the optional
-- capture of what a statement actually returned — unsealed, which is precisely
-- the exfiltration evidence an investigation leans on.
--
-- The chain here is one per *query*, mirroring the reasoning behind the
-- per-connection query chain: DBB_QUERY_STORAGE_RETENTION deletes whole
-- queries and query_rows.query_id cascades, so a per-query chain is severed
-- exactly when its parent goes away and never by housekeeping.
--
-- There is no chain_seq column: row_number is already the capture's own
-- ordering, so it is the chain position. It is *not* dense — a row the writer
-- had to drop, or one that failed to encode, leaves a gap — so the chain is
-- linked by prev_mac over the rows that were actually stored, and a gap is not
-- treated as a break. Removing a row from the middle is still caught, because
-- the next surviving row's prev_mac no longer matches its new predecessor.
ALTER TABLE query_rows
    ADD COLUMN prev_mac BYTEA,
    ADD COLUMN mac BYTEA;

--bun:split

-- The final head of a capture's row chain, stamped when the capture finishes
-- (the flush barrier the query waits on before it is marked complete).
-- Without it, deleting the *last* captured rows would leave a shorter chain
-- that still verified end to end; with it, the surviving rows no longer
-- compute the head the query claims.
--
-- row_chain_len is a count of chained rows, not the head's row_number: the two
-- differ exactly when the capture has gaps.
--
-- NULL/0 on a query that captured nothing, on rows written before this
-- migration, and on a capture whose process died before the barrier — the same
-- structural gap connections.query_chain_mac has for a session that never
-- closed cleanly.
ALTER TABLE queries
    ADD COLUMN row_chain_mac BYTEA,
    ADD COLUMN row_chain_len BIGINT NOT NULL DEFAULT 0;
