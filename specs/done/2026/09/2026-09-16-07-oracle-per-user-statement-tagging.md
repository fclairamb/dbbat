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

**Verdict on the cost: the per-user tag is affordable.** The question this spec
set out to answer — "does a tag constant per dbbat user fragment the shared
cursor cache?" — has a number now, and the answer is no: bounded by k, one-time,
sub-megabyte. The folklore is settled.

`shared.QueryTagger`'s existing constructor is indeed all that is needed to
produce it: `uidSuffix(uuid.Nil)` returns `""` and `appendQueryTagField` skips
empty values, so `NewQueryTagger(version, user, uuid.Nil, grant)` emits
`/*dbbat='…',user='…',grant='…'*/ ` with no `conn=` field and no new encoding
code. Verified and pinned by tests as `shared.NewUserQueryTagger`, a named
wrapper so the omission is intentional at the call site rather than an accident
of passing a zero value.

## What blocks the injection (and why nothing was wired)

The tag is not wired to Oracle, and `DBB_QUERY_TAGGING_ORACLE` was deliberately
**not** added. The spec's implementation sketch assumed an injection point of
the shape MySQL and PostgreSQL have — `upstreamText(sql)` at the last moment
before the write. Oracle has no such point, and that is a design invariant
rather than an oversight (`reassembly.go`): *"the buffered packets are forwarded
as they arrived, byte for byte: the reassembled buffer is for reading only, and
dbbat never synthesizes wire bytes toward the upstream."* The proxy **decodes**
the statement to gate and record it and relays the client's own TNS packets
untouched. PostgreSQL and MySQL are one-liners because those proxies re-encode
every message anyway; Oracle does not.

Prepending 40-odd bytes therefore means writing a TTC statement re-encoder:

- **Three statement-carrying ops**, each with its own SQL-length field:
  the v315+ piggyback exec `03 5e` and the JDBC `11 69` (TTC compressed int,
  whose *encoded width* changes with the value, shifting everything behind it),
  and `OALL8` (`decodeVarLen`: 1 byte / `0xFE`+2 / `0xFF`+4, with a bind count
  and the bind values sitting immediately behind the text).
- **A second, unrelated encoding for OCI clients** (sqlplus, SQL*Developer,
  Instant Client): the length is `sqlLen * 3` as a little-endian ub4 behind a
  `fe x8` pointer sentinel, and OCI sometimes counts a trailing NUL in it.
- **The CLR framing**: go-ora repeats the length as a raw byte immediately
  before the text, so that byte moves too — and a statement that crosses 252
  bytes changes *format*, from the short form to the `0xFE`-chunked long form.
  A tag is exactly what pushes a statement near that boundary across it.
- **The TNS frame**: `encodeTNSPacket` writes only the legacy 2-byte length
  header, so it cannot even reproduce a v315+ data packet (4-byte length, the
  2-byte field zeroed); and a message already at the negotiated SDU has to be
  re-fragmented.
- **The locator is a heuristic.** `locateExecSQLText` *searches* for a text run
  of the declared length, accepting `sqlLen` or `sqlLen-1` and a possible
  one-byte shift past a printable CLR prefix. That is sound for gating, which
  fails open to a scan. Rewriting a length prefix found by search is not: the
  documented failure mode of getting a TTC length field wrong, already met on
  the AUTH leg, is `ORA-03146 invalid buffer length for TTC field` — a dead
  session, in exchange for a comment.

Doing part of it is worse than none: a tag applied on some frames and not others
gives one statement *both* a tagged and an untagged SQL_ID, doubling the very
cursor count this measurement was about, with coverage varying by client.

So the honest split is: this spec answered its question, and the encoder is its
own piece of work — filed as `2026-09-16-08-oracle-ttc-statement-rewrite.md`.
Landed here: the measurement, `shared.NewUserQueryTagger` with the bytes pinned
by tests, and the number written into `docs/oracle.md` and the configuration
docs in place of the standing "would defeat the shared-cursor cache" folklore.
