---
model: opus
effort: high
---

# Statements forwarded by dbbat carry no identity of their own, so the database's own query tooling attributes them to the shared role

## Problem

The upstream session name (`application_name`, `program_name`) identifies the
dbbat user in `pg_stat_activity`, and the previous spec adds the connection to
it. It does not reach the places a DBA actually looks when a database is
slow:

- **RDS Performance Insights** groups by SQL digest and shows the SQL text; it
  shows the database user (`prod_datalake`, the shared role every dbbat
  session logs in as) and the client host (the dbbat proxy). That is exactly
  what the 2026-09-15 investigation saw: 93% of load "from one host", with no
  dbbat user in sight.
- **`pg_stat_statements`** keys on `(userid, dbid, queryid)`; `application_name`
  is not a dimension. Florent in the thread: "je ne sais pas si on le track
  dans les pg_stat_statements à ce stade... mais on pourrait".
- **Slow-query logs** (`log_min_duration_statement`, MySQL slow log) print the
  statement text, not the session's application name.

All three show the statement text. Google's
[sqlcommenter](https://google.github.io/sqlcommenter/) convention exists for
this: a `/*key='value',key2='value2'*/` comment carried on the statement. APMs
and Performance Insights already know how to display it.

## Proposal

### Opt-in, two protocols

`DBB_QUERY_TAGGING` (bool, default `false`; koanf `query_tagging.enabled`).
PostgreSQL and MySQL/MariaDB only in this version: both accept a leading
comment on every statement kind including `COPY`, `PREPARE`, `EXPLAIN` and
`CALL`, and both are where the tooling above lives. Oracle and SQL Server are
out (Oracle `V$SQL` deduplicates on text, so a per-connection tag would defeat
its shared-cursor cache; a per-user tag might be acceptable later). MongoDB
gets a natural equivalent through the `comment` field on commands, a follow-up.

Off by default because it changes the bytes the database receives, and a
deployment that pins statement text (a `pg_stat_statements` allowlist, a
query-firewall, a per-statement cache) should turn it on knowingly.

### Tag

Prepended, not appended:

```sql
/*dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris-habitat'*/ SELECT ...
```

sqlcommenter appends, but `pg_stat_activity.query` and the slow log truncate
at `track_activity_query_size` (1024 bytes by default), so an appended tag on
a long statement is the part that gets cut. Values are URL-encoded per the
sqlcommenter spec (the grant slug and the username are already slug-safe;
encode anyway). Keys: dbbat version, user, connection suffix (same 12 hex
characters as the app name), grant definition slug. No timestamps or per-query
ids: identical statements must stay identical so `pg_stat_statements` and the
MySQL digest keep aggregating them.

### Where the text changes

Only on the wire to upstream, after every dbbat control has run on the
original text:

- PostgreSQL: `pgproto3.Query.String` in the simple path and
  `pgproto3.Parse.Query` in the extended path (a prepared statement is tagged
  once, at Parse; every Execute inherits it). Both are built after
  `validateStatement` and `holdIfNeeded` in `internal/proxy/postgresql/intercept.go`.
  `COPY ... FROM STDIN` is tagged like any statement; the data stream is not
  touched.
- MySQL: `COM_QUERY` and `COM_STMT_PREPARE` payloads in `runIntercepted`
  (`internal/proxy/mysql/intercept.go:184`). `COM_STMT_EXECUTE` is binary and
  untouched.

### What must not change

- **The audit chain and the queries table record the client's text**, not the
  tagged one. The tag is deterministic from `(version, user, connection,
  grant)`, all of which the connection row already stores, so it is
  reproducible without being stored. Document that in `docs/audit-chain.md`.
- Read-only guards, `block_ddl`, `block_copy`, approval patterns and the
  keyword scans all run on the original text, before tagging. A pattern
  author never sees the tag.
- Query-text search in the UI matches the original text.
- Packet captures (`docs/dump-format.md`) record what dbbat saw and sent, so
  the upstream leg of a capture shows the tag. That is correct and needs one
  sentence in the doc.
- MySQL `max_allowed_packet`: the tag is ~90 bytes; a statement within 90
  bytes of the limit will now fail. Accept and document.

### Known limit

`pg_stat_statements` stores the text of the *first* execution of a digest. Two
dbbat users running the same statement share a row whose text carries the
first user's tag, so the row's identity is misleading; Performance Insights
has the same property per digest. `pg_stat_activity`, the slow log and PI's
per-sample text are exact. Say so in the docs; the alternative (a per-user
salt that breaks aggregation) is worse.

### Tests

- Tag builder unit tests: encoding, key order, stability across calls.
- PG integration: with tagging on, `pg_stat_activity.query` for a running
  `pg_sleep` starts with the tag; the `queries` row does not contain it; a
  `read_only` refusal and an approval hold behave identically with tagging on
  and off; a prepared statement round-trips through Parse/Bind/Execute; `COPY
  FROM STDIN` still loads. With tagging off, bytes upstream are unchanged
  (assert against a recorded exchange).
- MySQL integration: the slow log or `performance_schema.events_statements_current`
  shows the tag; `COM_STMT_PREPARE` + execute works.
- `dbbat audit verify --queries` passes on a store written with tagging on.

### Documentation

`CLAUDE.md` env table, `website/docs/configuration/index.md`,
`docs/postgresql.md` and `docs/mysql.md` with the Performance Insights and
`pg_stat_statements` caveats above.

## Implementation Plan

Ordered, committable steps. Each one builds and lints on its own.

1. **Config flag.** `QueryTaggingConfig{Enabled bool}` under koanf
   `query_tagging.enabled`, default `false`. `envTransform` maps the bare
   `DBB_QUERY_TAGGING` onto `query_tagging.enabled` (exact-match rule, like
   `connection_retention`), plus a `query_tagging_` prefix rule for a
   config-file-shaped `DBB_QUERY_TAGGING_ENABLED`. Config unit test pinning
   both mappings and the default-off.
   *Commit: `feat(config): add DBB_QUERY_TAGGING`*

2. **Tag builder** in `internal/proxy/shared/querytag.go`. `QueryTag` value
   (version/user/conn uid/grant slug) with `Prefix()` returning
   `/*dbbat='…',user='…',conn='…',grant='…'*/ `, values URL-encoded
   (`url.QueryEscape`), fixed key order, no clock and no counter anywhere in
   it — two calls on the same inputs must return the same bytes. `conn=`
   reuses `appname.go`'s `uidSuffix` (last 12 hex chars) so it matches the
   app-name `c=` tag. `Apply(sql)` returns sql unchanged when the tag is
   inert (no username *and* no connection *and* no grant) or when sql is
   empty. Unit tests: encoding of `'`/space/`*/`, key order, stability,
   idempotent inputs.
   *Commit: `feat(proxy): add the sqlcommenter-style statement tag builder`*

3. **`store.AccessGrant.DefinitionSlug()`** accessor — "" for a shapeless
   grant, mirroring the other definition accessors.
   *(folded into step 2's commit or its own)*

4. **PostgreSQL injection.** `Session.queryTag` built at auth (user, conn
   uid, grant slug) and only when the server's flag is on; `Server.
   SetQueryTagging(bool)` + `session.queryTagging = s.queryTagging` in
   `handleConnection`, wired from `main.go`. Tag applied at the very end of
   `handleQuery` (mutating `pgproto3.Query.String`) and of `handleParse`
   (mutating `pgproto3.Parse.Query`) — after `validateStatement`, after
   `holdIfNeeded`, and after the original text has been stored on
   `pendingQuery.sql` / `preparedStatement.sql`. Nothing else in the session
   reads the mutated field.
   *Commit: `feat(proxy): tag PostgreSQL statements with the dbbat identity`*

5. **MySQL injection.** `Server.SetQueryTagging(bool)`, `Session.queryTag`
   built at auth. `HandleQuery`'s exec closure runs the tagged text
   (`upstreamConn.Execute`) while `runIntercepted` keeps recording the
   original; `HandleStmtPrepare` prepares the tagged text while recording
   `PREPARE: <original>`. `COM_STMT_EXECUTE` is untouched — it inherits the
   tag baked in at prepare.
   *Commit: `feat(proxy): tag MySQL statements with the dbbat identity`*

6. **Docs.** `CLAUDE.md` env table, `website/docs/configuration/index.md`,
   `docs/postgresql.md`, `docs/mysql.md` (incl. the `max_allowed_packet`
   note and the `pg_stat_statements` first-execution-text limitation),
   `docs/audit-chain.md` (the chain records the client's text) and
   `docs/dump-format.md` (the upstream leg of a capture shows the tag).
   *Commit: `docs: document DBB_QUERY_TAGGING`*

7. **Tests.** Unit: the tag builder (run). Integration (`//go:build
   integration`, authored + compiled, not run here): PG tagging on →
   `pg_stat_activity.query` starts with the tag while the `queries` row does
   not contain it; `read_only` refusal and approval hold identical on/off;
   Parse/Bind/Execute round-trip; `COPY FROM STDIN` still loads; tagging off
   → no `/*dbbat=` upstream. MySQL: `performance_schema.
   events_statements_history` shows the tag, `COM_STMT_PREPARE` + execute
   works. Plus `audit verify --queries` clean with tagging on.
   *Commit: `test: cover statement tagging`*
