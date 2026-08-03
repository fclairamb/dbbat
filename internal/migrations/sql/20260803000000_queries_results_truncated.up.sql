-- Result capture stops at max_result_rows / max_result_bytes and keeps the
-- prefix it already has. Without this flag, a short row set is indistinguishable
-- from a query that genuinely returned that many rows — and an empty one from a
-- query that returned nothing at all.
ALTER TABLE queries ADD COLUMN results_truncated BOOLEAN NOT NULL DEFAULT FALSE;
