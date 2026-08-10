---
sidebar_position: 8
sidebar_label: Tamper-Evident Audit Log
title: Tamper-Evident Audit Log
description: DBBat HMAC-chains its audit log, query history and captured result rows, so modifying, deleting or reordering a record is detectable with dbbat audit verify.
---

# Tamper-Evident Audit Log

Logging everything is only half the story. The other half is being able to show
that nobody edited the log afterwards — including the person who runs the
database it lives in.

DBBat chains its records with a keyed MAC. Every audit entry, every statement in
a session's query history, and every row of a captured result set carries an
HMAC over its own content plus the previous record's MAC. Change a record,
delete one, swap two around, and the chain no longer adds up. `dbbat audit
verify` walks it and names the first record that broke.

The key is derived from DBBat's own encryption key (`DBB_KEY` /
`DBB_KEYFILE`) and never leaves the process — it is never stored in the
database and never logged. That is what makes this different from a plain hash
chain: someone who rewrites a row can recompute a hash, but not an HMAC.
**Direct access to DBBat's PostgreSQL store is not enough to forge the chain.**

Nothing to turn on. Chaining is always active.

## Verifying

```bash
# The administrative audit log: users, servers, grants, definitions, API keys
dbbat audit verify

# The per-connection query history
dbbat audit verify --queries

# A single session
dbbat audit verify --queries --connection 019fe8bb-b9d5-74ab-b512-601b6eccda98

# The captured result rows, per query (optionally scoped to one session)
dbbat audit verify --rows
```

They all need the same `DBB_DSN` and encryption key the server runs with. The
command exits non-zero if the chain does not verify, so it drops straight into
a cron job or a CI check.

A clean run reports the chain head:

```json
{"level":"INFO","msg":"Audit chain verified","entries":1284,"head_seq":1284,
 "head_mac":"9f2c…","unverifiable_pre_anchor_entries":37}
```

:::tip Record the head MAC outside the database

A chain always verifies against itself, so someone with the key could truncate
it and re-seal it. Writing `head_mac` down somewhere else — an evidence file, a
ticket, a signed note — is what closes that hole: the entry count must only
grow, and the head you recorded last quarter must still be in the chain today.

:::

When something is wrong, the command names the first bad record and stops —
everything after a break is meaningless anyway:

```
AUDIT CHAIN BROKEN break="chain_seq 412, row 019fe8bb-…: mac does not
match the entry's content: the entry was modified"
```

## Verifying over the API

The same walk is reachable over REST, so an evidence script does not need shell
access to the host:

```bash
# The administrative audit chain
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/audit/verify"

# The per-connection query chains
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/audit/verify/queries"

# A single session (also reports that chain's head)
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/audit/verify/queries?connection=019fe8bb-b9d5-74ab-b512-601b6eccda98"
```

```json
{"chain":"audit","verified":true,"entries":1284,"head_seq":1284,
 "head_mac":"9f2c…","unverifiable_pre_anchor_entries":37,
 "checked_at":"2026-08-10T09:14:02Z","cached":false}
```

The captured result rows are **CLI-only** for now: there is no
`/audit/verify/rows` endpoint, so `dbbat audit verify --rows` is the way to
check them.

Both endpoints require the **admin** role — narrower than the `GET /api/v1/audit`
list a viewer may read. A broken chain still answers `200`, with
`"verified": false` and a `break` object naming the first bad record: the
failure is in the data, not in the request. The response carries counts,
positions, the head MAC and the break reason, and never the chain key or the
content of any record.

:::warning The API answer is not equivalent to the CLI

`dbbat audit verify` runs where the key lives and can be run by someone who
does **not** trust the running server. `GET /api/v1/audit/verify` is served *by*
that server — a compromised or modified DBBat can return `"verified": true`
without walking anything, and the caller cannot tell.

And because a walk's outcome is cached for a minute, **the answer can be up to
60 seconds old**: a chain broken moments ago keeps reporting
`"verified": true` until that cached walk expires. Treat the endpoint as a
monitoring signal, not a point-in-time attestation — `dbbat audit verify` is the
instrument for the latter.

Use the endpoint for routine evidence collection, and the CLI (or an
independent re-run of it from a trusted binary against the store directly) when
the integrity of the DBBat process itself is part of what is being assessed.
Do not present the two as interchangeable in a control narrative.

:::

A chain walk is `O(rows)`, so an instance remembers each walk's outcome for a
minute and runs at most one walk at a time. `cached` tells you whether an answer
was reused and `checked_at` when it was actually computed — poll for a fresh
number, not a fresh timestamp.

## What it detects

| Tampering | Detected | How |
|---|---|---|
| A record's content is **modified** | Yes | Its own MAC no longer matches its content |
| A record is **deleted** from the middle | Yes | The successor's `prev_mac` no longer matches, and its position leaves a gap |
| Records are **reordered** | Yes | Positions and `prev_mac` links no longer line up |
| The **first** records are deleted | Yes | The first entry's `prev_mac` is a genesis MAC derived from the key, which cannot be forged |
| The **last** statements of a closed session are deleted | Partly — see the warning below | The session's final chain head is stamped on the connection row when it closes, but that stamp is not itself sealed |
| The **last** rows of a captured result set are deleted | Yes | The capture's final head is sealed onto the query row with a keyed MAC when the capture finishes, so correcting it needs the key |
| A captured result set is deleted **outright** | Yes | The sealed stamp on the query still attests to rows no longer there |
| The **whole** chain is truncated and re-sealed by someone holding the key | Only against a head MAC you recorded elsewhere | See the tip above |

:::warning A trailing deletion on the query chain can be covered up

Deleting the **last statements of a session** is caught by the chain head
stamped on the connection row — but that stamp is a plain copy of the last
statement's MAC, and that value is readable from the query history itself. So
someone with write access to DBBat's storage database can delete the last
statements *and then rewrite the stamp to match*, and the verification reports a
clean chain. No key is needed.

It catches the careless case, not the deliberate one. The same is **not** true of
the captured result rows: their stamp is a keyed MAC, so it cannot be corrected
without `DBB_KEY`.

If trailing-deletion detection on the query history is load-bearing for a
control, say so explicitly and lean on the recorded head MAC (below) or on
shipping the logs to a WORM store or SIEM as well. Fixing the stamp format is
[tracked](https://github.com/fclairamb/dbbat/blob/main/specs/todos/2026-08-10-06-seal-the-connection-query-chain-stamp.md);
it is not a one-line change because stamps in the old format already exist in
deployed installations.

:::

## What it does not do

Be precise about this in a control narrative:

- **It detects tampering; it does not prevent it.** This is evidence, not
  enforcement. Locking down direct access to DBBat's storage database is still
  your job.
- **It does not cover records written before the feature shipped.** Upgrading
  inserts an anchor row marking where chaining begins. Rows older than the
  anchor have no MAC and none can be created after the fact, so verification
  reports them separately as `unverifiable_pre_anchor_entries` instead of
  counting them as verified.
- **On the query side it seals what ran, not how it went.** The MAC covers a
  statement's identity — the SQL text, the bind parameters, the execution time,
  the connection and the position in the session. It does not cover the outcome
  columns (duration, rows affected, error, approval resolution), because those
  are written after the statement is logged, and re-sealing them would
  invalidate every record chained after it.
- **On the captured rows it seals what was stored, not that the capture was
  complete.** Every column of a captured row is covered, but the
  `results_truncated` / `results_dropped` flags on the parent query are written
  after the result set has been read — like the outcome columns above — so they
  are not sealed either. A capture whose DBBat process died before its rows were
  finalised also carries no head stamp, so only its beginning and middle are
  protected.
- **A crashed session's tail is sealed late, not at the crash.** A session whose
  DBBat process died never gets to record its own chain head. The reconcile that
  closes crash-orphaned sessions recovers it from the stored statements instead,
  and stamps it in the same write that marks the session disconnected — so the
  tail *is* protected, but from the reconcile onward, not from the crash.
  Statements deleted in between are sealed as if they had never been written.
  The window is however long it takes DBBat to notice the process is gone: the
  reclaim runs at startup and then every few minutes, and a process that was
  killed outright has to miss heartbeats for fifteen minutes first.
- **It does not defend against someone who has the key.** Anyone who can read
  `DBB_KEY` off the host can rewrite the store and re-seal it. Treat that key
  the way you treat the database credentials it protects.

## Retention does not break it

The query chain is per connection, not one global chain across the whole table.
That is deliberate: `DBB_QUERY_STORAGE_RETENTION` deletes old history, and a
single global chain would break the first time it ran.

- Retention deleting a whole connection takes that connection's chain with it.
  Every other connection still verifies end to end.
- Retention deleting the oldest statements of a long-running, still-open session
  truncates that chain's beginning. Verification reports it as a truncated
  prefix — counted, not flagged as tampering — and keeps verifying everything
  after it.

Captured result rows follow the same logic one level down: their chain is per
query, and retention deletes whole queries, so a reaped capture takes its own
chain with it. Nothing legitimate ever deletes an *individual* captured row —
which is why a capture missing its first rows is reported as tampering rather
than as housekeeping.

## Where it fits in an audit

The chain is the mechanism behind **ISO/IEC 27001:2022 A.8.15** (the integrity
half of logging) and **PCI DSS v4.0 requirement 10.3** — audit logs are
protected from destruction and unauthorized modifications. See
[Compliance](/docs/compliance) for the full mapping, and the
[design note](https://github.com/fclairamb/dbbat/blob/main/docs/audit-chain.md)
for the cryptography, the canonical serialization and the concurrency model.

## See also

- [Query Logging](/docs/features/query-logging) — what is recorded in the first place
- [Compliance](/docs/compliance) — the control mappings
- [Security](/docs/security) — the rest of DBBat's cryptography
