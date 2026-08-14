# Tamper-evident audit chain

DBBat's audit trail is HMAC-chained: every audit entry, every statement in a
session's query history, and every row of a captured result set carries a MAC
over its own content plus the previous record's MAC. Altering, deleting or
reordering a record breaks the chain, and `dbbat audit verify` reports the first
record that does not add up.

The key is derived from DBBat's master encryption key and never leaves the
process. That is the difference between this and a plain hash chain: whoever
rewrites a row can recompute a hash, but cannot recompute an HMAC without the
key. **Read/write access to the PostgreSQL store is not enough to forge the
chain.**

## What it proves, and what it does not

It detects:

- an entry whose content was **modified**;
- an entry **deleted** from the middle of the chain;
- entries **reordered**;
- rows deleted from the **end** of a captured result set (the capture's final
  head is sealed onto the query row when the capture finishes);
- entries deleted from the **end** of a session's query history, including
  against an attacker who rewrites the stamp afterwards *or clears it outright*
  — the stamp is keyed, and its absence on a closed session that logged
  statements is itself a break (see [The connection
  stamp](#the-connection-stamp) and [A stamp that was
  cleared](#a-stamp-that-was-cleared)). A session that is still
  **open** is covered up to the last periodic sweep of its stamp, not up to its
  last statement (see [An open session is stamped by a periodic
  sweep](#an-open-session-is-stamped-by-a-periodic-sweep)). A session carrying
  the unkeyed stamp — only a store written by a pre-0.24 development build has
  any — attests to nothing and is reported as a break;
- **every** statement of a session deleted — not just its tail. The walk
  enumerates stamped connections as well as connections with surviving
  statements, so an emptied session is judged rather than skipped (see [A
  session emptied of every statement](#a-session-emptied-of-every-statement));
- a **whole session** deleted, connection row and all — but *only* through the
  two `audit_log` entries every session writes, never through the `connections`
  row itself, which is not chained (see [The session
  entries](#the-session-entries));
- entries deleted from the **start** of the audit chain (the first entry's
  `prev_mac` is a genesis MAC derived from the key, so it cannot be forged).

It does **not**:

- **prevent** tampering. It is evidence, not enforcement. Locking down direct
  access to the storage database is still your job.
- protect rows written **before the chain anchor**. The migration that
  introduced chaining inserts an anchor row; everything older has no MAC and is
  reported separately as unverifiable, rather than silently counted as
  verified.
- seal a query's **outcome**. See [Coverage](#coverage) below.
- seal the `connections` **row**. That table carries no MAC. A whole-session
  delete is detectable only by comparing the surviving rows against the
  `connection.opened` / `connection.closed` entries in `audit_log`; nothing in
  `dbbat audit verify` performs that comparison for you, and every column of a
  connection row — `connected_at` included — can still be edited in place
  without breaking any chain. The two entries record the immutable half of that
  row, so an edit is *detectable by comparison*; it is not detected by a walk.
  See [The session entries](#the-session-entries).
- protect against someone who has the **key**. An attacker who reads
  `DBB_KEY` / `DBB_KEYFILE` off the host can rewrite the store and re-seal it.
  This is why the head MAC is meant to be recorded outside the database (see
  [Running a verification](#running-a-verification)).

## Keying

```
chain key = HKDF-SHA256(master = DBB_KEY, salt = "", info = "dbbat-audit-chain-v1", 32 bytes)
MAC       = HMAC-SHA256(chain key, canonical serialization)
```

`internal/crypto/derive.go`. The `info` string is versioned: changing the
canonical serialization means bumping it, which changes the key, which makes
two chain generations trivially distinguishable instead of silently
interchangeable. The derived key is never written to the database and never
logged.

A store built without an encryption key writes unchained rows and refuses to
verify (`audit chain key is not configured`). A serving process cannot land
there: config always resolves a key, creating `~/.dbbat/key` if it has to.

## The three chains

### `audit_log` — one chain for the store

Administrative events: user, server, grant, grant-definition, grant-request and
API-key changes. Low volume, insert-only, never deleted by retention — so it is
a single global chain, and every column is covered.

Columns added by `20260810000000_audit_chain`:

| Column | Meaning |
|---|---|
| `chain_seq` | Position in the chain. `0` is the anchor, `1..n` are real entries, `NULL` means the row predates chaining |
| `prev_mac` | The previous entry's MAC; for `chain_seq = 1`, a genesis MAC derived from the key |
| `mac` | HMAC over this entry |

Ordering is `chain_seq`, **not** `uid`. UUIDv7 only orders by millisecond, and
two entries minted in the same millisecond have no defined order — which a
chain cannot tolerate.

#### The session entries

`audit_log` also carries two entries per proxied session, and they are there for
a reason that has nothing to do with administration:

```sql
DELETE FROM connections WHERE uid = '…';
```

`queries.connection_id` and `query_rows.query_id` are both `ON DELETE CASCADE`,
so one statement removes a session, everything it ran and everything it read.
Every *other* deletion is covered — middle, start, end, and the whole of a
session's statements — but nothing references a connection row once it is gone,
so this one used to leave no trace at all.

Chaining `connections` was the obvious answer and was rejected. That table is
one retention deletes from, which is exactly why the query chain is split per
connection in the first place, and a global chain over it would report a
truncated prefix after every sweep — worse, `disconnected_at` is what the sweep
orders by while `connected_at` is what the chain would, so a long-lived session
breaks the assumption that a sweep only ever removes the oldest links.

So instead: `Store.CreateConnection` writes a `connection.opened` entry and every
writer that closes a session (`CloseConnection`, and the reconcile's
`CloseOrphanedConnections` / `ReclaimDeadInstanceConnections`) writes a
`connection.closed` one. `audit_log` is already chained and is never reaped by
retention, so the evidence survives in a table the delete does not touch, and no
new chain has to be reconciled against `CleanupOldQueryRows`.

Each entry's `details` carries the connection row's **immutable identity** — the
connection uid, the user, the database, the source IP, `connected_at`, the
instance and run stamps, and the grant it authenticated under. The close entry
adds `disconnected_at`, who closed it (`session` or `reconcile`) and the
session's `query_chain_mac` / `query_chain_len` / `query_chain_stamp_version`, so
the sealed record points at the query chain that session owned. The mutable
counters — `last_activity_at`, `queries`, `bytes_transferred` — are deliberately
out: they keep changing after the entry is sealed, so recording them could only
ever produce a record that disagrees with the row, exactly like the query chain's
outcome columns.

**Be precise about what this buys.** It makes a whole-session delete *detectable
by comparison* — the entries name a session no `connections` row accounts for —
and it makes an edited connection row detectable the same way, `connected_at`
included. It does **not** seal the `connections` row: `dbbat audit verify`
performs no such comparison, and every column of that table is still an unchained
column anyone with write access can rewrite. The one place that matters inside
verification is the retention excuse of [A session emptied of every
statement](#a-session-emptied-of-every-statement), which reads `connected_at`:
backdating it buys the excuse, and only on a deployment that actually sets
`DBB_QUERY_STORAGE_RETENTION` — with retention off, the default, `queryRetention
<= 0` and no session is excused at all, so there is nothing to buy.

**Two writes, not fatal ones.** A failed audit write is logged and the session
carries on; the store must not be able to take a live database session down over
a log line. Both entries are written *after* the state they describe is
committed, so an entry can be missing but never fabricated, and neither goes
inside the caller's transaction: the chain append owns its own — it takes the
store-wide advisory lock and retries on a stale head, and a colliding INSERT
aborts whatever transaction it is in. Nesting it would invert the rule (a lost
entry rolling back a completed close) and would hold every closed connection row
while contending with every admin action in the store. The reconcile, which
closes an arbitrary number of sessions at once, appends its entries as one
chained batch per 500 connections (`Store.LogAuditEvents`) rather than one round
trip each.

**Volume, and why they are hidden by default.** Two entries per session, in a
table retention never reaps, is a real growth rate: a proxy serving ten thousand
sessions a day writes twenty thousand of these against a handful of grant, user
and key changes. Folded into the audit listing they would push every
control-plane event off the first page within seconds, so `GET /api/v1/audit`
leaves them out of an unfiltered listing — `?event_type=connection.closed`
returns them, the connections list is the purpose-built surface for browsing
sessions, and a partial index
(`20260812000000_audit_log_control_plane_index`) keeps the default page costing
its own length rather than the ratio between the two kinds of event. Nothing
about the chain changes: these are ordinary chained rows, walked and verified
like every other.

### `queries` — one chain per connection

The wire audit is where the compliance value peaks, but the table is
high-volume and `DBB_QUERY_STORAGE_RETENTION` deletes from it. A single global
chain would break the first time retention ran. So each statement's MAC covers
the previous statement **on the same connection**, and:

- retention deleting a whole connection takes that connection's chain with it
  and leaves every other connection verifiable end to end;
- retention deleting the oldest statements of a still-open, long-lived session
  truncates that chain's *prefix*. That is expected housekeeping, not tampering:
  verification reports it as a truncated prefix and keeps verifying everything
  after it.

On a clean session close, the final chain head and its length are sealed onto
the connection row (`query_chain_mac`, `query_chain_len`,
`query_chain_stamp_version`). Without that, deleting the *last* statements of a
session would leave a shorter chain that still verified. See
[The connection stamp](#the-connection-stamp) for the construction and for what
an unkeyed row still means.

There are **three** writers of that stamp, and only the first two are closes:

| writer | when | what it seals |
|---|---|---|
| `CloseConnection` | clean teardown | the session's final head |
| the reconcile (`CloseOrphanedConnections` / `ReclaimDeadInstanceConnections`) | a crashed session is closed | whatever survived at reconcile time |
| `RefreshOpenChainStamps` | every reclaim tick, for sessions **still open** | the head as of that sweep — a *prefix* |

#### An open session is stamped by a periodic sweep

A close is a bad moment to be the only moment. A session that never ends — a
`psql` window left open all day, a pooled application connection, an approval
hold waiting on a human — used to carry no stamp at all for its entire life, so
deleting its most recent statements was undetectable, and stayed undetectable
until it closed, at which point the close sealed the already-truncated chain.
A crashed session had the same hole from the crash until the reclaim noticed it.

`Store.RefreshOpenChainStamps` rides on the reclaim tick
(`InstanceReclaimInterval`, ~7.5min, spread) and re-seals the head of every
session **this run** still has open: select the open uids where
`run_id = <this run>`, read their heads with the same `LEFT JOIN LATERAL`
lookup the reconcile uses, seal in Go, write back in one `UPDATE`. It is scoped
to this run's rows so replicas sharing a store never contend over the same
stamps, and the write is guarded by `disconnected_at IS NULL` so a sweep that
read a head before a concurrent `CloseConnection` committed can never land
afterwards and overwrite the exact final stamp with an older one.

Cost: one index lookup per open session per pass. It scales with concurrency,
never with the size of the query history — pinned by
`TestQueryChainRefreshCostScalesWithOpenSessions`, which EXPLAINs both halves.

**The stamp never moves backwards.** Once a live session carries a sweep's
stamp, the next writer is either its close or — if the process crashes — the
reconcile, and both recover the head from what is *left in the database*.
Someone who deleted the tail in between would otherwise have the later writer
overwrite the sweep's higher stamp with their truncated one. Chain positions
only grow and retention removes the oldest, so a recovered head below the stored
stamp has exactly one meaning; every stamp write is therefore conditional on
`new length >= stored length`, and the higher stamp stays put so verification
reports the break.

**A live session's stamp is a prefix, and verification knows it.** Between two
sweeps a busy session runs statements the stamp does not cover, so while
`disconnected_at IS NULL` the stamp is checked against the statement at the
`chain_seq` it *names*, not against the current head. Judging it exactly would
report a break on every active session in the store, which is worse than not
refreshing at all. The rule, in full:

| stamped `chain_seq` vs the surviving chain | open session | closed session |
|---|---|---|
| equals the head | verified | verified |
| below the head, that statement survives | verified against **that** statement | **break** — a close is final |
| below the oldest survivor | retention reaped it: unverifiable, already counted as a truncated prefix | **break** |
| above the head | **break** — retention only removes the oldest | **break** |
| nothing survives at all | **break**, unless retention could have reaped every statement — see below | same |
| no stamp at all, statements survive | no sweep has reached it yet: verified | **break** — a close always stamps a session that logged statements |
| no stamp at all, nothing survives | the session logged nothing: verified | same |
| no stamp but `query_chain_len` survives | **break** — the three stamp columns are only ever written together | same |

**What this does not buy.** The stamp only ever proves the chain up to the
position it sealed, so statements appended *since the last sweep* are still
unprotected against a trailing deletion. The refresh bounds that window by the
sweep interval instead of by the session's lifetime; it does not close it. The
alternative — stamping from the append path every N statements — would tighten
it further at the cost of a second write on the hot path, and was not taken.

#### A crash-orphaned session is stamped by the reconcile

A session whose process died never reaches `CloseConnection`, and the in-memory
chain state died with it. It gets a stamp anyway: the head is recoverable from
the database alone — it is the highest `chain_seq` on the connection and its MAC
— so the reconcile that closes crash-orphaned rows
(`CloseOrphanedConnections` / `ReclaimDeadInstanceConnections`) seals it in the
**same transaction** that writes `disconnected_at`: the close returns the uids
it took, their heads are read back in one `LEFT JOIN LATERAL` lookup, and the
sealed stamps go back in one `UPDATE`. A connection that logged nothing keeps a
NULL stamp — which is legitimate precisely because it logged nothing; on a
closed session whose statements survive, a NULL stamp is a break (see [A stamp
that was cleared](#a-stamp-that-was-cleared)).

**Be honest about what that stamp attests to.** It seals whatever survived *at
reconcile time*, not what the session actually wrote. A crashed pod's rows sit
unstamped from the crash until the reclaim notices — no registry row, or one
past the 15-minute staleness cutoff — and anyone who deletes trailing statements
inside that window gets the truncated chain blessed by the reconcile. That is
strictly better than never stamping at all (from the reconcile onward the tail
is protected exactly like a clean close's), and it is the reason the stamp goes
in the same transaction rather than a later pass: the exposure is the crash-to-
reconcile window, not a second one opened between closing the row and sealing it.
An earlier revision of the reconcile stamped in one pure-SQL `UPDATE`, reading `mac` out of
`queries` with a correlated subquery — which is a verbatim copy by construction.
A keyed stamp has no SQL expression, because the chain key exists only in the
process, so that path became select-seal-write.

#### The connection stamp

```
query_chain_mac = HMAC(chain key,
    "dbbat-query-chain-stamp-v1" ‖ connection_uid ‖ stamp_version ‖ query_chain_len ‖ head_mac)
```

Every field is tagged and length-prefixed by the same canonical writer the row
MACs use, so no two distinct inputs can produce the same byte string.

`query_chain_len` is the head's `chain_seq`, not a count of surviving
statements. `chain_seq` is dense from 1, so it *is* the number of statements the
session logged — and unlike a survivor count it does not move when retention
reaps the oldest ones. Sealing a survivor count would report a break on every
retention-truncated session, which is exactly the cry-wolf the truncated-prefix
handling exists to avoid. (The row-chain stamp *can* seal a true count, because
retention never deletes individual captured rows.) For a closed session,
verification compares the recorded length against what the surviving statements
say before it checks the MAC, so an edit to the column alone is caught with a
precise message; for an open one that comparison becomes the prefix rule above.

**Two formats, and the version is inside the MAC.** An earlier revision of this
feature stored the last statement's MAC **verbatim**, and that value is readable
from the `queries` table — so the attacker this feature is built against could
delete the last statements of a session, copy the new last statement's MAC into
the stamp, and get a clean verification with no key at all.

Such rows cannot be re-sealed: the chain key never enters the database, so no
migration can rewrite them. `connections.query_chain_stamp_version` says which
format a row is in — `0` unkeyed, `1` keyed — and **only version `1` verifies**:

- version `1`: the keyed stamp, computed from what the surviving statements say;
- version `0`: a **break**, with its own reason — the tail cannot be verified,
  because that stamp is a copy of a value anyone with write access can read and
  rewrite. The break does not accuse anyone of deleting anything; it says the
  stamp attests to nothing.

The version is covered by the version-1 MAC, which is why the column is worth
having at all. Without that, relabelling a sealed row as version `0` would be
one `UPDATE` away from getting the unkeyed rule applied to a session this build
sealed. With it, a keyed stamp only ever verifies as the version it was sealed
at.

**Why version 0 is not simply tolerated, and why there is no way to ask for it
to be.** It was tolerated, in development, and it was a standing downgrade path:
an attacker could delete the tail of a *sealed* session and replace the whole
stamp — raw head MAC, matching length, version back to `0` — and verification
accepted it, costing them nothing but a counter they were betting nobody
watched. 0.24 ships the chain, the keyed stamp and the version column together,
so no released build ever had the door open, and no upgrade path arrives here
carrying version-0 rows: only a store written by a **pre-0.24 development
build** can hold one. Such a store is unverifiable and stays that way — nothing
can re-seal those rows, and re-creating a dev store is cheap.

#### A stamp that was cleared

Forging a stamp needs the key. **Removing** one does not, and that was the whole
attack the stamp's other hardening never touched:

```sql
DELETE FROM queries WHERE connection_id = … AND chain_seq > 7;
UPDATE connections SET query_chain_mac = NULL, query_chain_len = 0 WHERE uid = …;
```

The surviving statements are all self-consistent, the walk found no stamp to
compare them against, and `dbbat audit verify --queries` reported the session
verified. Every hardening the stamp received — the keyed format, the sealed
version, the end of the version-0 acceptance — raised the cost of *defeating*
the check while leaving the cost of *deleting* it at one `UPDATE`. So a NULL
stamp is now judged rather than skipped.

A NULL stamp now means three different things, and only two of them are
innocent:

| the connection | verdict |
|---|---|
| logged nothing (no chained statement survives) | not a break: no writer stamps a NULL head, so this is the expected state |
| is still **open** | not a break: `RefreshOpenChainStamps` runs on the reclaim tick, so a session younger than one sweep has no stamp yet |
| is **closed** and its chained statements survive | **break** |

The third is a state no writer produces. `CloseConnection` seals the head in the
same `UPDATE` that writes `disconnected_at`, and the reconcile seals it in the
same transaction — so a closed session that logged statements always carries a
stamp.

`query_chain_len > 0` beside a NULL MAC gets its **own** reason, and applies to
an open session too: all three writers set the MAC, the length and the version
in one statement, so a length that outlived its MAC is not a stamp that was
never written. It is a row somebody wrote to.

**There is no escape hatch here, deliberately.** `query_chain_mac` and the
close-path writer that fills it arrived in the *same* release as the chain
itself, so no released dbbat ever closed a chained session without stamping it,
and a connection predating the chain migration has no stamp *and* no chained
statement — the first row of the table, not the third. An opt-in here would also
be worse than one for the unkeyed version-0 stamp: a version-0 stamp is at least
a distinguishable state, whereas a NULL stamp is exactly what the attacker
writes, so tolerating it would not be a weaker check but the absence of this
one.

Retention cannot trip it either. `CleanupOldQueryRows` only ever deletes rows;
no sweep clears a stamp. A closed session it emptied keeps its stamp and is
judged by the next section instead.

#### A session emptied of every statement

An earlier revision of the walk enumerated connections out of `queries`
(`SELECT DISTINCT connection_id ...`), so a session with **no** surviving
statement produced no row, was never walked, and its stamp — claiming N
statements — was never compared against anything. Deleting all but one of a
session's statements was caught; deleting all of them was not. The row chain had
already closed the same hole one level down, which is why `capturedQueryUIDs`
unions in `row_chain_mac IS NOT NULL`.

`chainedConnectionUIDs` now enumerates from `connections` instead: a connection
is in scope when it carries a stamp **or** still has chained statements. The
second half needs no `UNION` — `queries.connection_id` is `ON DELETE CASCADE`,
so no statement outlives its connection row. A session that logged nothing has
no stamp and is still skipped.

That leaves the judgment. A stamped session with zero surviving statements has
two possible causes, and `queries` alone cannot tell them apart:

1. `DBB_QUERY_STORAGE_RETENTION` reaped them all. The connection sweep only
   reaps rows whose `disconnected_at` is already past the cutoff, so a session
   that went quiet long before it closed — a pooled connection, an idle
   `psql` — keeps its row while the query sweep empties it. A session still
   **open** is the same story: the sweep never reaps a live connection row.
2. Someone deleted them.

What separates them is time, and the only thing that says where the line runs is
the configured retention window itself, which the store is given at startup
(`store.Options.QueryRetention`, from the same `DBB_QUERY_STORAGE_RETENTION` the
sweep reads). The sweep deletes by `executed_at < now - retention`, and every
statement of a session ran at or after its `connected_at` — so a session that
**connected at or after the cutoff** cannot have had a single statement reaped,
and an empty one is a break. With retention disabled, which is the default, the
cutoff is the beginning of time and nothing is excused. An excused session is
reported as a truncated prefix (the extreme of one), never silently skipped.

Two consequences worth stating plainly:

- The rule is the *sound* one, not the tight one. A session that connected
  before the cutoff but ran its statements after it is excused too. Closing that
  gap would need the deleted statements' timestamps — which is exactly what was
  deleted. And `connected_at` is itself a plain column on an unchained table, so
  an attacker who can `DELETE FROM queries` can equally
  `UPDATE connections SET connected_at` to buy the excuse. That is only worth
  anything where the excuse exists: with `DBB_QUERY_STORAGE_RETENTION` unset —
  the default — nothing is excused and the backdating buys nothing. Where a
  retention *is* set, the session's `connection.opened` entry carries the
  `connected_at` it really had, chained; comparing the two is what catches the
  edit, and no walk does it for you.
- Verification reads the retention from configuration rather than from the data
  (say, the oldest statement left in the store), because a young or quiet store
  has its oldest surviving statement minutes old, which would excuse nearly
  every session. The cost is one caveat: **raising or disabling
  `DBB_QUERY_STORAGE_RETENTION` moves the cutoff backwards**, so sessions the
  previous setting legitimately emptied can start reading as breaks. Lowering it
  never can.

An emptied session carrying an **unkeyed** stamp never reaches this rule: it is
already a break for its stamp, and it is reported that way rather than as a
deletion. An unkeyed stamp attests to nothing either way, so a session retention
emptied and one somebody emptied are the same bytes, and no key separates them.

### `query_rows` — one chain per capture

The optional capture of what a statement actually returned
(`max_result_rows` / `max_result_bytes`) is the evidence an exfiltration
investigation leans on, so it is chained too. The shape follows the same
reasoning as the query chain, one level down: retention deletes whole queries
and `query_rows.query_id` is `ON DELETE CASCADE`, so **one chain per query** is
severed exactly when its parent goes away and never by housekeeping.

Two things differ from the chains above.

**There is no `chain_seq`.** `row_number` is already the capture's own ordering,
so it *is* the chain position. It is deliberately **not** required to be dense:
the batched row writer drops rows when its queue is full, and a row that fails
to encode is skipped, so gaps are normal. Density is not what makes a deletion
detectable — the `prev_mac` linkage is. Removing a row from the middle leaves
its successor pointing at a MAC no surviving row has. `row_chain_len` is
therefore a *count* of stored rows, not the head's `row_number`; the two differ
exactly when the capture has gaps.

**A missing prefix is a break.** Retention never deletes an individual captured
row — it deletes whole queries and whole connections — so unlike a long-lived
session's oldest statements, a capture that no longer starts at its genesis link
has been tampered with, and verification says so.

The head is stamped on the *query* row (`row_chain_mac`, `row_chain_len`) at the
capture's flush barrier: the point where every captured row is durable and the
query is about to be marked complete (`QuerySink.Flush` →
`Store.SealQueryRowChain`).

**That stamp is itself a MAC**, not a copy of the head:

```
row_chain_mac = HMAC(chain key, "dbbat-row-chain-stamp-v1" ‖ query_uid ‖ row_chain_len ‖ head_mac)
```

Storing the head verbatim — which is what an earlier revision of
`connections.query_chain_mac` did — would defend against nothing, because the head is readable straight out of
`query_rows`: whoever deletes the last captured rows can copy the new last row's
MAC over the stamp. Sealing it means correcting the stamp after a truncation
needs the chain key, exactly like forging a row. Verification checks both halves
against what the surviving rows compute, the recorded length as well as the MAC,
so editing either one is a break. A capture whose process died before the barrier
keeps a NULL stamp and only has its prefix and interior protected — structurally
the same gap `connections.query_chain_mac` has between a crash and the reconcile
that stamps it. Verification enumerates queries that carry a stamp as well as queries
with surviving rows, so a capture deleted *outright* still gets caught by its
orphaned stamp instead of vanishing from the walk.

## Coverage

The audit chain covers every column of an `audit_log` row: `uid`, `event_type`,
`user_id`, `performed_by`, `details`, `created_at`, `chain_seq`, `prev_mac`.

The query chain covers a statement's **immutable identity** only: `uid`,
`connection_id`, `chain_seq`, `sql_text`, `parameters`, `executed_at`,
`prev_mac`. It deliberately does not cover the outcome columns — `duration_ms`,
`rows_affected`, `error`, and the approval resolution — because those are
written *after* the row is inserted. A MAC covering them would have to be
recomputed on completion, which would invalidate the `prev_mac` every successor
already points at.

So the query chain proves **which statements ran, with which parameters, in
which order**. It does not seal their reported outcome.

The row chain covers every column of a `query_rows` row — `uid`, `query_id`,
`row_number`, `row_data`, `row_size_bytes`, `prev_mac` — because that table is
insert-only: nothing is ever written to a captured row after its INSERT.

What it deliberately does **not** reach is the capture's after-the-fact metadata
on the parent query. `results_truncated` and `results_dropped` are written by
`UpdateQueryCompletion` once the result set has been read, exactly like
`duration_ms` and `rows_affected`, and sealing them would mean recomputing a MAC
whose successors already point at it. That is the same call the query chain made
for the outcome columns, and the row chain follows it. So the row chain proves
**which rows were captured, with what content, in what order**; it does not seal
the claim that the capture was complete.

## Determinism

A MAC computed when a row is written has to be reproducible from that row read
back out of PostgreSQL, forever. Two things make that non-obvious, and both are
handled in `internal/store/chain_canonical.go`:

1. **Timestamps.** `timestamptz` stores microseconds; Go keeps nanoseconds. A
   timestamp that is not truncated before it is hashed never hashes the same
   again. DBBat truncates to microseconds and normalizes to UTC *before* the
   insert, so the stored value is exactly the hashed value.

2. **`jsonb` is not text.** PostgreSQL parses `details` and `parameters`, sorts
   the keys, drops the whitespace, keeps only the last of duplicate keys and
   re-renders the numbers (`1e2` comes back as `100`). Hashing the raw bytes
   would report a break on the first read. DBBat hashes a *canonical* form —
   keys sorted, no insignificant whitespace, numbers reduced to a plain decimal
   that depends only on their value — chosen so that
   `canonical(jsonb(x)) == canonical(x)`. A store test verifies that fixed-point
   property against a real PostgreSQL over a battery of documents.

Fields are written length-prefixed and tagged, so no two different field
sequences can produce the same byte string, and an absent (NULL) field hashes
differently from an empty one.

## Concurrency

The chain head is a serialization point: two appends that read the same head
would write the same `chain_seq` and the same `prev_mac`, and one would be
lost. Each append therefore runs inside a transaction holding a PostgreSQL
advisory lock — one lock for the audit chain, one per connection for the query
chains — with the head cached in memory and re-read under the lock whenever the
cache is cold or an append failed. A partial unique index on `chain_seq` is the
backstop.

With several replicas sharing one store, a cached head goes stale as soon as a
peer appends: this process thinks the head is 5, the peer already wrote 6, and
the insert collides with that unique index. An append therefore retries — the
first attempt trusts the cache, every later one re-reads the head under the lock
— so the event is never lost to a race between replicas.

Audit volume is admin actions, so the contention this buys is negligible. Query
appends only ever contend with other appends on the *same* connection, which is
a single session.

The row chains are the exception that shaped their write path: a single batch
from the shared row writer spans several queries and therefore several chains,
and that path is the hottest writer in the store. Splitting the batch per query
is **not** allowed to become a round trip per query, so one transaction covers
the whole batch — one statement takes every chain lock it needs (in query-uid
order, so two overlapping batches cannot deadlock), one statement loads whatever
heads are not cached, and the INSERT stays a single bulk INSERT spanning every
query in the batch. Batching still amortizes the round trip across ~1000 rows;
what chaining costs is the transaction plus those two statements *per batch*.

## Running a verification

```bash
# The administrative audit chain
dbbat audit verify

# The per-connection query chains
dbbat audit verify --queries

# One session
dbbat audit verify --queries --connection 019fe8bb-b9d5-74ab-b512-601b6eccda98

# The per-capture result row chains (optionally scoped to one session)
dbbat audit verify --rows
dbbat audit verify --rows --connection 019fe8bb-b9d5-74ab-b512-601b6eccda98
```

`--queries` and `--rows` are different chains; passing both is refused rather
than silently picking one.

Both need the same `DBB_DSN` and `DBB_KEY` / `DBB_KEYFILE` the server runs
with. The command exits non-zero when the chain does not verify.

A clean audit run reports the head:

```json
{"level":"INFO","msg":"Audit chain verified","entries":1284,"head_seq":1284,
 "head_mac":"9f2c…","unverifiable_pre_anchor_entries":37}
```

**Record `head_mac` somewhere outside the database** — a ticket, a signed note,
an evidence file. A chain always verifies against itself, so truncating it and
re-sealing is undetectable *from the inside*; comparing today's head against the
one you recorded last quarter is what closes that hole. `entries` should only
ever grow, and the previously recorded head must still appear in the chain.

`unverifiable_pre_anchor_entries` counts rows written before chaining was
introduced. They are reported rather than folded into the verified count: no
MAC exists for them and none can be created after the fact.

A `--queries` run reports the same way, plus a count that is *not* a failure:

```json
{"level":"INFO","msg":"Query chains verified","connections":412,"statements":9871,
 "chains_with_retention_truncated_prefix":3}
```

`chains_with_retention_truncated_prefix` is what `DBB_QUERY_STORAGE_RETENTION`
leaves behind on a long-lived session — housekeeping, and everything after the
truncation is still verified.

### Over the API

All three chains are checkable over REST:

| Endpoint | Scope filters | Reports a head |
|---|---|---|
| `GET /api/v1/audit/verify` | — | always (one store-wide chain) |
| `GET /api/v1/audit/verify/queries` | `?connection=<uid>` | scoped walk only |
| `GET /api/v1/audit/verify/rows` | `?connection=<uid>` or `?query=<uid>` | `?query=` only |

They run the same walkers as the CLI and return the same numbers as JSON. All
are **admin-only** — narrower than the `GET /api/v1/audit` list a viewer may
read. A broken chain is still a `200`, with `"verified": false` and a `break`
object; the failure is in the data, not the request. Handler:
`internal/api/audit_verify.go`.

On `/audit/verify/rows`, `?connection=` and `?query=` cannot be combined: a
query already names exactly one capture, so a second filter could only agree or
contradict, and silently ignoring one is how a caller ends up trusting the
answer to a question it did not ask. The row response has no
`chains_with_truncated_prefix` field, and deliberately so — retention never
removes an individual captured row, so a missing prefix is a break rather than
housekeeping.

**No endpoint reports an unkeyed-stamp count.** A version-0 stamp is always a
break, everywhere and with no opt-out, so there is nothing tolerated to count
and a field that could only ever report `0` would be worse than no field. The
admin audit panel in the UI follows the same rule.

Two properties are load-bearing and have tests pinning them:

- **The response never carries the chain key or a record's content.** It carries
  counts, chain positions, the head MAC and the human-readable break reason. A
  MAC is meant to be published; the key never leaves the process.
- **A walk is O(rows), so it is bounded.** Each scope's outcome is cached for
  `chainVerifyTTL` (a minute) and at most one walk runs at a time per instance,
  so an admin hammering the endpoint reads memory instead of scanning the table.
  A `?since_seq` window was considered and rejected: resuming a walk from a
  caller-supplied position means trusting that row's `prev_mac`, which is
  exactly the value an attacker who rewrote the chain controls.

**The endpoint is not equivalent to the CLI, and the docs say so.** It is served
by the process under audit: a compromised dbbat can answer `"verified": true`
without walking anything. The CLI can be run by someone who does not trust that
process. The caching adds a second, smaller gap: an answer is up to 60 seconds
old, so a chain broken moments ago keeps reporting `"verified": true` until the
cached walk expires — the endpoint is a monitoring signal, the CLI is the
point-in-time attestation. Both `website/docs/features/audit-chain.md` and
`website/docs/compliance.md` state this where an assessor will read it —
overselling the endpoint is the failure mode for a compliance-facing feature.

A broken chain names the first bad record:

```
AUDIT CHAIN BROKEN break="chain_seq 412, row 019fe8bb-…: mac does not match the entry's content: the entry was modified"
```

Everything after a break is meaningless, so the walk stops there.

## Compliance framing

The chain is the mechanism behind **ISO/IEC 27001:2022 A.8.15** (the integrity
half of logging) and **PCI DSS v4.0 requirement 10.3** (audit logs protected
from destruction and unauthorized modifications). Be precise in a control
narrative: it *detects* modification by someone without the key, it does not
prevent it, and it says nothing about rows written before the anchor. See
`website/docs/compliance.md`.

## Key files

- `internal/crypto/derive.go` — HKDF subkey derivation
- `internal/store/chain.go` — keying, payload construction, head caches
- `internal/store/chain_canonical.go` — the canonical serialization
- `internal/store/chain_verify.go` — the walkers
- `internal/api/audit_verify.go` — the admin-only REST endpoints and the cache
  that bounds them
- `internal/store/audit.go`, `internal/store/queries.go`,
  `internal/store/connections.go` — the write paths
- `internal/store/connection_audit.go` — the per-session `audit_log` entries,
  and why `connections` is not chained
- `internal/proxy/shared/rowwriter.go` — the flush barrier that seals a capture
- `heartbeat.go` — the reclaim tick that also runs the open-session stamp sweep
- `internal/migrations/sql/20260810000000_audit_chain.*.sql`,
  `internal/migrations/sql/20260810010000_query_row_chain.*.sql`,
  `internal/migrations/sql/20260810020000_connections_query_chain_stamp_version.*.sql`,
  `internal/migrations/sql/20260812000000_audit_log_control_plane_index.*.sql`
- `main.go` — the `audit verify` command
