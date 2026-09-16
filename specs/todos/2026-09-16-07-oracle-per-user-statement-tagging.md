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

## Measurement

Run on 2026-09-16 against a real instance: `gvenzl/oracle-free:23-slim`,
reporting `Oracle AI Database 26ai Free Release 23.26.3.0.0`, service
`FREEPDB1`, `cursor_sharing=EXACT`, `session_cached_cursors=50`,
`open_cursors=300`, automatic shared-pool sizing.

One statement — a two-table join with a `GROUP BY`, two bind variables and a
real plan, not `SELECT 1` — executed **600 times per arm** over **one** physical
session, with `ALTER SYSTEM FLUSH SHARED_POOL` between arms. The only thing that
varies across arms is how many *distinct texts* those 600 executions are spread
over, which is exactly what the tag changes. `V$SQL` is aggregated over every
cursor matching the statement; `V$LIBRARYCACHE` deltas are on the `SQL AREA`
namespace.

| arm | distinct texts | `V$SQL` SQL_IDs | child cursors | `LOADS` | `PARSE_CALLS` | `SHARABLE_MEM` | SQL AREA `GETMISSES` | `RELOADS` |
|---|---|---|---|---|---|---|---|---|
| untagged | 1 | 1 | 1 | 1 | 600 | 48 KB | 10 | 13 |
| per-user (`user=`, k=20) | 20 | 20 | 20 | 20 | 600 | 962 KB | 44 | 7 |
| per-user, 2nd pass, no flush | 20 | 20 | 20 | **20** | 1200 | **962 KB** | **0** | **0** |
| per-conn (`user=`+`conn=`, 200 sessions) | 200 | 200 | 200 | 200 | 600 | 9 624 KB | 404 | 7 |

Reading:

- **The per-user cost is one-time and bounded by k, not by traffic.** `LOADS`
  equals the number of distinct texts in every arm — one hard parse per
  identity, and every execution after that is a soft parse against that
  identity's own cursor. The second pass makes it explicit: 600 more executions
  on an un-flushed pool added **zero** loads, **zero** library-cache gets,
  **zero** getmisses, **zero** reloads, and not one byte of `SHARABLE_MEM`. It
  plateaus at k exactly as hoped, rather than degrading per execution.
- **The shared-pool footprint is ~48 KB per distinct identity** (962 KB / 20).
  For a fleet with 20 dbbat users that is under a megabyte of shared pool per
  hot statement — noise against any real SGA.
- **The per-connection tag is the one that does not land**, and by the same
  measurement: 200 sessions of the same statement cost 200 cursors and 9.6 MB,
  and unlike k it has no ceiling — it grows with every new session. Spec 05 was
  right to refuse it, and this number is the evidence.
- No arm produced any `invalidations`, and `RELOADS` did not rise with the
  number of identities (13 / 7 / 7), so nothing here is aging cursors out.

**Verdict: the per-user tag lands.** Implemented behind its own
`DBB_QUERY_TAGGING_ORACLE` setting (`off` by default), separate from
`DBB_QUERY_TAGGING`.

`shared.QueryTagger`'s existing constructor is indeed all that is needed:
`uidSuffix(uuid.Nil)` returns `""` and `appendQueryTagField` skips empty values,
so `NewQueryTagger(version, user, uuid.Nil, grant)` emits
`/*dbbat='…',user='…',grant='…'*/ ` with no `conn=` field and no new code. A
named constructor wraps it so the omission is intentional at the call site
rather than an accident of passing a zero value.
