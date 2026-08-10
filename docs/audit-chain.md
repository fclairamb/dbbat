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
- entries deleted from the **end** of a session's query history — but only
  against an attacker who does not also rewrite the stamp; see
  [The connection stamp is forgeable](#the-connection-stamp-is-forgeable);
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

On a clean session close, the final chain head and its length are stamped onto
the connection row (`query_chain_mac`, `query_chain_len`). Without that,
deleting the *last* statements of a session would leave a shorter chain that
still verified. A session that died without closing has no stamp; the startup
reconcile that closes crash-orphaned connections has no chain state to stamp.

#### The connection stamp is forgeable

`query_chain_mac` stores the last statement's MAC **verbatim**, and that value
is readable from the `queries` table. So the attacker this whole feature is
built against — someone with write access to the store but without the key —
can delete the last statements of a session and then copy the new last
statement's MAC into the stamp, and verification reports a clean chain. Nothing
else covers it: `queryChainPayload` seals a statement's identity and does not
reach the connection row, so editing the stamp does not break the query chain
either. `query_chain_len` is likewise stored and printed but never compared.

So the trailing-deletion guarantee on the *query* chain only holds against an
attacker who stops after the delete. The row chain does not have this problem —
its stamp is keyed, below — and the fix for the connection stamp is
`specs/todos/2026-08-10-seal-the-connection-query-chain-stamp.md`. It is a
separate task because stamps in the old format already exist in shipped
deployments, so changing the format needs a compatibility story.

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

Storing the head verbatim — which is what `connections.query_chain_mac` does —
would defend against nothing, because the head is readable straight out of
`query_rows`: whoever deletes the last captured rows can copy the new last row's
MAC over the stamp. Sealing it means correcting the stamp after a truncation
needs the chain key, exactly like forging a row. Verification checks both halves
against what the surviving rows compute, the recorded length as well as the MAC,
so editing either one is a break. A capture whose process died before the barrier
keeps a NULL stamp and only has its prefix and interior protected — structurally
the same gap `connections.query_chain_mac` has for a session that never closed
cleanly. Verification enumerates queries that carry a stamp as well as queries
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

### Over the API

`GET /api/v1/audit/verify` and `GET /api/v1/audit/verify/queries` (the latter
takes an optional `?connection=<uid>`) run the same walkers and return the same
numbers as JSON. **The row chains are CLI-only for now** — there is no
`/audit/verify/rows` endpoint yet. Both are **admin-only** — narrower than the `GET /api/v1/audit`
list a viewer may read. A broken chain is still a `200`, with
`"verified": false` and a `break` object; the failure is in the data, not the
request. Handler: `internal/api/audit_verify.go`.

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
- `internal/proxy/shared/rowwriter.go` — the flush barrier that seals a capture
- `internal/migrations/sql/20260810000000_audit_chain.*.sql`,
  `internal/migrations/sql/20260810010000_query_row_chain.*.sql`
- `main.go` — the `audit verify` command
