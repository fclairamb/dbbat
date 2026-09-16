---
model: opus
effort: medium
---

# Oracle statements are untaggable per connection, but a per-user tag may still be worth it

## Goal

Decide, with evidence rather than by assumption, whether Oracle can carry a
*coarser* dbbat identity tag — one that is constant per dbbat **user** rather
than per connection — and implement it if the shared-cursor cost is acceptable.

## Why

Spec `2026-09-16-05-sql-comment-tagging` deliberately left Oracle out: `V$SQL`
keys on the statement text, so a tag carrying `conn='<12hex>'` gives every
dbbat connection its own SQL_ID for the same statement. On a busy instance
that is not a cosmetic problem — it fragments the shared cursor cache, inflates
the library cache and can push an instance into hard-parse territory. The spec
noted, without deciding, that "a per-user tag might be acceptable later".

It is worth deciding rather than leaving as folklore: Oracle is where Stonal's
largest shared-role databases are, so it is where "93% of load from one host"
hurts most, and `/*dbbat='0.28.1',user='florent'*/` multiplies cursors by the
number of *users*, not by the number of *sessions*.

## Implementation

- **Measure first.** On a real Oracle (the `make test-e2e-oracle` container, or
  a Stonal RDS instance through the cluster harness per
  `project_oracle_test_harness`), run one statement N times from k distinct
  tagged identities and count `V$SQL` child cursors and library cache misses.
  The deliverable of this step is a number in the spec, not a code change.
- If it lands: a separate setting, not a silent widening of
  `DBB_QUERY_TAGGING` — the trade-off is Oracle-specific and an operator who
  turned tagging on for PostgreSQL did not consent to it. Something like
  `DBB_QUERY_TAGGING_ORACLE=user`, with `off` the default.
- `shared.QueryTagger` needs a variant that omits `conn=` (it already omits
  empty fields, so passing `uuid.Nil` produces exactly the per-user tag —
  verify that is all that is needed).
- Injection point: the TTC statement text in `internal/proxy/oracle/`, after
  every control has run on the original — the same invariant the SQL proxies
  hold, with the same test asserting the `queries` row is untagged.
- If it does not land: record the measurement in `docs/oracle.md` and close
  this out. A documented number is a better outcome than a standing "maybe".
