---
model: opus
effort: medium
---

# MongoDB commands carry no dbbat identity, so Atlas profiler output attributes them to the shared user

## Goal

Give the MongoDB proxy the equivalent of `DBB_QUERY_TAGGING`: identify the
dbbat user, connection and grant in the target's own slow-operation output
(`system.profile`, the Atlas profiler, `db.currentOp()`), the way
`/*dbbat='…',user='…',conn='…',grant='…'*/` now does on PostgreSQL and MySQL.

## Why

Spec `2026-09-16-05-sql-comment-tagging` shipped statement tagging for
PostgreSQL and MySQL only, and named MongoDB an explicit follow-up: it has no
SQL comments, but it has a documented, first-class equivalent — the `comment`
field, accepted on `find`, `aggregate`, `update`, `delete`, `getMore` and
others, and echoed back by the profiler and `currentOp`. Until then a MongoDB
session through dbbat is exactly the "93% of load from one host, no dbbat user
in sight" problem the PostgreSQL spec was written for.

## Implementation

- Reuse `shared.QueryTagger`'s inputs, not its output: the SQL comment string
  is wrong here. Add something like `QueryTagger.Fields()` returning the same
  four values, and build a BSON document (or the same `key='value'` string —
  decide which reads better in the Atlas UI; a string is what sqlcommenter
  consumers expect).
- Inject in `internal/proxy/mongodb/intercept.go`, on the `OP_MSG` body, after
  every dbbat control has run on the **original** command — same invariant as
  the SQL proxies: the `queries` row, the audit chain and the approval-hold
  patterns must all keep seeing the client's command.
- **A client-supplied `comment` wins.** Unlike a SQL comment, `comment` is a
  single-valued field a client may already be using (Mongo drivers and ORMs
  set it), and silently overwriting it would break their own tracing. Decide
  and document: append into a sub-document, or skip tagging that command.
- Same opt-in flag (`DBB_QUERY_TAGGING`) — this is the same feature, not a
  second one. `main.go`'s `startProxies` already gates PG and MySQL there.
- Tests mirroring the SQL ones: an integration test reading the tag back out
  of `system.profile`, and the "the store holds the client's command, not the
  tagged one" assertion.
- Docs: `docs/mongodb.md`, plus the MongoDB line in the `DBB_QUERY_TAGGING`
  rows of `CLAUDE.md` and `website/docs/configuration/index.md`, which
  currently say MongoDB is out of scope.
