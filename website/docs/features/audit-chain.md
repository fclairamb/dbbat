---
sidebar_position: 8
sidebar_label: Tamper-Evident Audit Log
title: Tamper-Evident Audit Log
description: DBBat HMAC-chains its audit log and query history, so modifying, deleting or reordering a record is detectable with dbbat audit verify.
---

# Tamper-Evident Audit Log

Logging everything is only half the story. The other half is being able to show
that nobody edited the log afterwards — including the person who runs the
database it lives in.

DBBat chains its records with a keyed MAC. Every audit entry, and every
statement in a session's query history, carries an HMAC over its own content
plus the previous record's MAC. Change a record, delete one, swap two around,
and the chain no longer adds up. `dbbat audit verify` walks it and names the
first record that broke.

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
```

Both need the same `DBB_DSN` and encryption key the server runs with. The
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
| The **last** statements of a closed session are deleted | Yes | The session's final chain head is stamped on the connection row when it closes |
| The **whole** chain is truncated and re-sealed by someone holding the key | Only against a head MAC you recorded elsewhere | See the tip above |

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
