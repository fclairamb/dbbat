-- Result rows are persisted through a batched, bounded writer. When that
-- writer falls behind (its queue is full) or a batch insert fails, rows are
-- dropped so the customer's query is never stalled by dbbat's own storage.
--
-- This is deliberately NOT results_truncated: truncated means a configured
-- capture limit was reached (an expected, explainable prefix), dropped means
-- dbbat lost rows it intended to keep. Both make a capture partial, for
-- opposite reasons, and conflating them would make the UI lie.
ALTER TABLE queries ADD COLUMN results_dropped BOOLEAN NOT NULL DEFAULT FALSE;
